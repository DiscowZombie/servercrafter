package resolve

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/template"
	"github.com/sirupsen/logrus"
)

const (
	defaultMavenExtension = "jar"
	mavenMetadataFile     = "maven-metadata.xml"
	mavenSnapshotSuffix   = "-SNAPSHOT"
)

// Version keywords asking for a discovery pass instead of naming a version
const (
	VersionLatest          = "latest"
	VersionRelease         = "release"
	VersionLatestRelease   = "latest-release"
	VersionLatestSnapshot  = "latest-snapshot"
	VersionLatestPublished = "latest-published"
)

// mavenRepo reads a standard Maven repository layout: GitLab, Nexus, Reposilite, Maven Central…
type mavenRepo struct {
	client *fetch.Client
	log    logrus.FieldLogger
	base   string
	auth   fetch.Auth
}

func (m *mavenRepo) resolve(ctx context.Context, d *template.Download) (*Resolved, error) {
	extension := d.Extension
	if extension == "" {
		extension = defaultMavenExtension
	}
	coordinates := d.GroupID + ":" + d.ArtifactID
	version, err := m.resolveVersion(ctx, d.GroupID, d.ArtifactID, d.Version)
	if err != nil {
		return nil, err
	}
	fileVersion := version
	if isMavenSnapshot(version) {
		// Every deploy of a snapshot publishes a file stamped with a time and a build number
		// (core-1.0.0-20260730.120000-2.jar) under the same -SNAPSHOT directory: the metadata
		// published alongside tells which one is the newest
		metadata, err := m.metadata(ctx, d.GroupID, d.ArtifactID, version, mavenMetadataFile)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Snapshots deployed without unique names carry no metadata and are served under the
			// plain -SNAPSHOT file name
			m.log.WithError(err).Debugf("No snapshot metadata for %s:%s, using the non-unique file name", coordinates, version)
		} else if unique := metadata.snapshotVersion(version, d.Classifier, extension); unique != "" {
			fileVersion = unique
		} else {
			return nil, fmt.Errorf("no %s snapshot of %s:%s published for classifier %q", extension, coordinates, version, d.Classifier)
		}
	}
	file := mavenFileName(d.ArtifactID, fileVersion, d.Classifier, extension)
	u := m.url(d.GroupID, d.ArtifactID, version, file)
	checksums := fetch.Checksums{}
	for _, algorithm := range []string{"sha256", "sha1"} {
		digest, err := m.client.GetBytes(ctx, u+"."+algorithm, m.auth, 1024)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if fields := strings.Fields(string(digest)); len(fields) > 0 && isHex(fields[0]) {
			_ = checksums.Add(algorithm, fields[0])
			break
		}
	}
	return &Resolved{Origin: u, Version: fileVersion, Checksums: checksums, Open: m.client.HTTPOpener(u, m.auth, nil)}, nil
}

// resolveVersion turns a version keyword into the version it currently designates
func (m *mavenRepo) resolveVersion(ctx context.Context, groupID string, artifactID string, version string) (string, error) {
	keyword := strings.ToLower(version)
	switch keyword {
	case VersionLatest, VersionRelease, VersionLatestRelease, VersionLatestSnapshot:
	default:
		return version, nil
	}
	metadata, err := m.metadata(ctx, groupID, artifactID, mavenMetadataFile)
	if err != nil {
		return "", fmt.Errorf("resolve the %s version of %s:%s: %w", keyword, groupID, artifactID, err)
	}
	v := metadata.Versioning
	var resolved string
	switch keyword {
	case VersionLatest:
		resolved = coalesce(v.Latest, v.Release, last(v.Versions, func(string) bool { return true }))
	case VersionRelease, VersionLatestRelease:
		resolved = coalesce(v.Release, last(v.Versions, func(s string) bool { return !isMavenSnapshot(s) }))
	case VersionLatestSnapshot:
		if isMavenSnapshot(v.Latest) {
			resolved = v.Latest
		} else {
			resolved = last(v.Versions, isMavenSnapshot)
		}
	}
	if resolved == "" {
		return "", fmt.Errorf("%s:%s has no %s version", groupID, artifactID, keyword)
	}
	m.log.Debugf("Resolved %s:%s %s to %s", groupID, artifactID, keyword, resolved)
	return resolved, nil
}

func (m *mavenRepo) metadata(ctx context.Context, groupID string, artifactID string, parts ...string) (*mavenMetadata, error) {
	u := m.url(groupID, artifactID, parts...)
	body, err := m.client.GetBytes(ctx, u, m.auth, 16<<20)
	if err != nil {
		return nil, err
	}
	metadata := &mavenMetadata{}
	if err = xml.Unmarshal(body, metadata); err != nil {
		return nil, fmt.Errorf("parse %s: %w", u, err)
	}
	return metadata, nil
}

// url joins the repository base, the group id with its dots as separators, the artifact id and
// the path segments below it
func (m *mavenRepo) url(groupID string, artifactID string, parts ...string) string {
	segments := []string{strings.TrimSuffix(m.base, "/")}
	for _, segment := range strings.Split(groupID, ".") {
		segments = append(segments, url.PathEscape(segment))
	}
	segments = append(segments, url.PathEscape(artifactID))
	for _, part := range parts {
		segments = append(segments, url.PathEscape(part))
	}
	return strings.Join(segments, "/")
}

// mavenMetadata is the part of a maven-metadata.xml we read
type mavenMetadata struct {
	Versioning struct {
		Latest   string   `xml:"latest"`
		Release  string   `xml:"release"`
		Versions []string `xml:"versions>version"`
		Snapshot struct {
			Timestamp   string `xml:"timestamp"`
			BuildNumber int    `xml:"buildNumber"`
		} `xml:"snapshot"`
		SnapshotVersions []struct {
			Classifier string `xml:"classifier"`
			Extension  string `xml:"extension"`
			Value      string `xml:"value"`
		} `xml:"snapshotVersions>snapshotVersion"`
	} `xml:"versioning"`
}

// snapshotVersion returns the version the newest deploy gave to the file matching the classifier
// and the extension, empty when nothing published matches them
func (m *mavenMetadata) snapshotVersion(baseVersion string, classifier string, extension string) string {
	for _, published := range m.Versioning.SnapshotVersions {
		if published.Classifier == classifier && published.Extension == extension {
			return published.Value
		}
	}
	// snapshotVersions is what Maven 3 writes; older repositories only carry the build the last
	// deploy produced, which every file of that deploy shares
	snapshot := m.Versioning.Snapshot
	if len(m.Versioning.SnapshotVersions) == 0 && snapshot.Timestamp != "" {
		return strings.TrimSuffix(baseVersion, mavenSnapshotSuffix) +
			"-" + snapshot.Timestamp + "-" + strconv.Itoa(snapshot.BuildNumber)
	}
	return ""
}

func isMavenSnapshot(version string) bool {
	return strings.HasSuffix(strings.ToUpper(version), mavenSnapshotSuffix)
}

// mavenTimestampedSnapshot matches the version a deploy gives to a snapshot file
var mavenTimestampedSnapshot = regexp.MustCompile(`^(.+)-\d{8}\.\d{6}-\d+$`)

// mavenBaseVersion returns the version a file version was deployed under, folding a timestamped
// snapshot (1.0.0-20260730.120000-2) back to its -SNAPSHOT version
func mavenBaseVersion(version string) string {
	if m := mavenTimestampedSnapshot.FindStringSubmatch(version); m != nil {
		return m[1] + mavenSnapshotSuffix
	}
	return version
}

func mavenFileName(artifactID string, version string, classifier string, extension string) string {
	name := artifactID + "-" + version
	if classifier != "" {
		name += "-" + classifier
	}
	return name + "." + extension
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && len(s) >= 40
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func last(values []string, keep func(string) bool) string {
	for i := len(values) - 1; i >= 0; i-- {
		if keep(values[i]) {
			return values[i]
		}
	}
	return ""
}
