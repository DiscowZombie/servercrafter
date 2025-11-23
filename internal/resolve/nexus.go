package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/template"
	"github.com/sirupsen/logrus"
)

// nexusSource reads a Sonatype Nexus Repository: files by path from a repository, Maven artifacts
// through the search API, which picks the newest matching asset server side
type nexusSource struct {
	client     *fetch.Client
	log        logrus.FieldLogger
	base       string
	repository string
	auth       fetch.Auth
}

type nexusSearch struct {
	Items []struct {
		DownloadURL string            `json:"downloadUrl"`
		Path        string            `json:"path"`
		Checksum    map[string]string `json:"checksum"`
		Maven2      struct {
			Version     string `json:"version"`
			BaseVersion string `json:"baseVersion"`
			Classifier  string `json:"classifier"`
			Extension   string `json:"extension"`
		} `json:"maven2"`
	} `json:"items"`
}

func (n *nexusSource) fetchPath(_ context.Context, p string) (*Resolved, error) {
	if n.repository == "" {
		return nil, errors.New("the nexus source has no repository to read paths from")
	}
	u := joinURL(n.base+"/repository/"+url.PathEscape(n.repository), p)
	return &Resolved{Origin: u, Open: n.client.HTTPOpener(u, n.auth, nil)}, nil
}

func (n *nexusSource) maven(ctx context.Context, d *template.Download) (*Resolved, error) {
	repository := coalesce(d.Repository, n.repository)
	if repository == "" {
		return nil, errors.New("no repository: set it on the download or on the nexus source")
	}
	extension := coalesce(d.Extension, defaultMavenExtension)
	query := url.Values{}
	query.Set("repository", repository)
	query.Set("maven.groupId", d.GroupID)
	query.Set("maven.artifactId", d.ArtifactID)
	query.Set("maven.extension", extension)
	if d.Classifier != "" {
		query.Set("maven.classifier", d.Classifier)
	}
	query.Set("sort", "version")
	query.Set("direction", "desc")
	exact := false
	switch strings.ToLower(d.Version) {
	case VersionLatest:
	case VersionRelease, VersionLatestRelease:
		query.Set("prerelease", "false")
	case VersionLatestSnapshot:
		query.Set("prerelease", "true")
	default:
		// Nexus matches search parameters token by token, so "test" also finds
		// "feat-rollout-cmd-test": the results are filtered again below for an exact match
		exact = true
		query.Set("maven.baseVersion", d.Version)
	}
	u := n.base + "/service/rest/v1/search/assets?" + query.Encode()
	result := &nexusSearch{}
	if err := n.client.GetJSON(ctx, u, n.auth, result); err != nil {
		return nil, err
	}
	for _, item := range result.Items {
		// Without a classifier the search also returns the classified assets (sources, javadoc…)
		if item.Maven2.Classifier != d.Classifier || item.Maven2.Extension != extension {
			continue
		}
		if exact && coalesce(item.Maven2.BaseVersion, mavenBaseVersion(item.Maven2.Version)) != d.Version {
			continue
		}
		checksums := fetch.Checksums{}
		for _, algorithm := range []string{"sha256", "sha1"} {
			if err := checksums.Add(algorithm, item.Checksum[algorithm]); err != nil {
				return nil, err
			}
		}
		return &Resolved{
			Origin:    item.DownloadURL,
			Version:   item.Maven2.Version,
			Checksums: checksums,
			Open:      n.client.HTTPOpener(item.DownloadURL, n.auth, nil),
		}, nil
	}
	return nil, fmt.Errorf("nexus has no %s asset of %s:%s:%s (classifier %q) in %s",
		extension, d.GroupID, d.ArtifactID, d.Version, d.Classifier, repository)
}
