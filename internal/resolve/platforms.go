package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/template"
)

// --- PaperMC (Fill API v3): paper, folia, velocity, waterfall ---

type paperVersions struct {
	Versions []struct {
		Version struct {
			ID string `json:"id"`
		} `json:"version"`
		Builds []int `json:"builds"`
	} `json:"versions"`
}

type paperBuild struct {
	ID        int    `json:"id"`
	Channel   string `json:"channel"`
	Downloads map[string]struct {
		Name      string            `json:"name"`
		Checksums map[string]string `json:"checksums"`
		URL       string            `json:"url"`
	} `json:"downloads"`
}

var paperPrerelease = regexp.MustCompile(`(?i)-(pre|rc)`)

func paperChannels(channel string) map[string]bool {
	switch strings.ToLower(channel) {
	case "alpha":
		return map[string]bool{"STABLE": true, "BETA": true, "ALPHA": true}
	case "beta":
		return map[string]bool{"STABLE": true, "BETA": true}
	}
	return map[string]bool{"STABLE": true}
}

func (r *Resolver) paperMC(ctx context.Context, d *template.Download) (*Resolved, error) {
	base := strings.TrimSuffix(r.APIs.PaperMC, "/") + "/projects/" + url.PathEscape(d.Project)
	channels := paperChannels(d.Channel)
	var build *paperBuild
	var version string
	var err error
	switch {
	case d.Build != 0:
		version = d.Version
		build = &paperBuild{}
		err = r.http.GetJSON(ctx, fmt.Sprintf("%s/versions/%s/builds/%d", base, url.PathEscape(version), d.Build), nil, build)
	case d.Version == "" || strings.EqualFold(d.Version, VersionLatest):
		versions := &paperVersions{}
		if err = r.http.GetJSON(ctx, base+"/versions", nil, versions); err != nil {
			break
		}
		// Versions are listed newest first; pre-releases and versions with no build in the
		// accepted channels are skipped
		tried := 0
		for _, candidate := range versions.Versions {
			if len(candidate.Builds) == 0 || paperPrerelease.MatchString(candidate.Version.ID) {
				continue
			}
			if tried++; tried > 5 {
				break
			}
			if build, err = r.paperLatestBuild(ctx, base, candidate.Version.ID, channels); err != nil {
				return nil, err
			}
			if build != nil {
				version = candidate.Version.ID
				break
			}
		}
		if build == nil {
			err = fmt.Errorf("papermc: %s has no version with a %s build", d.Project, channelName(d.Channel))
		}
	default:
		version = d.Version
		if build, err = r.paperLatestBuild(ctx, base, version, channels); err == nil && build == nil {
			err = fmt.Errorf("papermc: %s %s has no %s build", d.Project, version, channelName(d.Channel))
		}
	}
	if err != nil {
		return nil, err
	}
	download, ok := build.Downloads["server:default"]
	if !ok {
		return nil, fmt.Errorf("papermc: build %d of %s %s has no server download", build.ID, d.Project, version)
	}
	checksums := fetch.Checksums{}
	if err = checksums.Add("sha256", download.Checksums["sha256"]); err != nil {
		return nil, err
	}
	return &Resolved{
		Origin:    download.URL,
		Version:   version + "-" + strconv.Itoa(build.ID),
		Checksums: checksums,
		Open:      r.http.HTTPOpener(download.URL, nil, nil),
	}, nil
}

// paperLatestBuild returns the newest build of version in the accepted channels, nil if none
func (r *Resolver) paperLatestBuild(ctx context.Context, base string, version string, channels map[string]bool) (*paperBuild, error) {
	var builds []paperBuild
	if err := r.http.GetJSON(ctx, base+"/versions/"+url.PathEscape(version)+"/builds", nil, &builds); err != nil {
		return nil, err
	}
	sort.SliceStable(builds, func(i, j int) bool { return builds[i].ID > builds[j].ID })
	for i := range builds {
		if channels[strings.ToUpper(builds[i].Channel)] {
			return &builds[i], nil
		}
	}
	return nil, nil
}

func channelName(channel string) string {
	if channel == "" {
		return "stable"
	}
	return strings.ToLower(channel)
}

// --- Modrinth ---

type modrinthVersion struct {
	ID            string `json:"id"`
	VersionNumber string `json:"version_number"`
	VersionType   string `json:"version_type"`
	Files         []struct {
		URL      string            `json:"url"`
		Filename string            `json:"filename"`
		Primary  bool              `json:"primary"`
		Hashes   map[string]string `json:"hashes"`
	} `json:"files"`
}

func (r *Resolver) modrinth(ctx context.Context, d *template.Download) (*Resolved, error) {
	query := url.Values{}
	if len(d.Loaders) > 0 {
		loaders, _ := json.Marshal(d.Loaders)
		query.Set("loaders", string(loaders))
	}
	if len(d.GameVersions) > 0 {
		gameVersions, _ := json.Marshal(d.GameVersions)
		query.Set("game_versions", string(gameVersions))
	}
	u := strings.TrimSuffix(r.APIs.Modrinth, "/") + "/project/" + url.PathEscape(d.Project) + "/version"
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var versions []modrinthVersion
	if err := r.http.GetJSON(ctx, u, nil, &versions); err != nil {
		return nil, err
	}
	wanted := d.Version
	if wanted == "" {
		wanted = VersionLatest
	}
	var match func(v *modrinthVersion) bool
	switch strings.ToLower(wanted) {
	case VersionLatest, VersionRelease, VersionLatestRelease:
		match = func(v *modrinthVersion) bool { return v.VersionType == "release" }
	case VersionLatestPublished:
		match = func(*modrinthVersion) bool { return true }
	default:
		match = func(v *modrinthVersion) bool { return v.VersionNumber == wanted || v.ID == wanted }
	}
	var asset *regexp.Regexp
	if d.Asset != "" {
		asset = regexp.MustCompile("^(?:" + d.Asset + ")$")
	}
	// Versions are listed newest first
	for i := range versions {
		v := &versions[i]
		if !match(v) {
			continue
		}
		index := -1
		for j, file := range v.Files {
			if asset != nil {
				if asset.MatchString(file.Filename) {
					index = j
					break
				}
			} else if file.Primary || index < 0 {
				index = j
				if file.Primary {
					break
				}
			}
		}
		if index < 0 {
			return nil, fmt.Errorf("modrinth: version %s of %s has no file matching %q", v.VersionNumber, d.Project, d.Asset)
		}
		file := v.Files[index]
		checksums := fetch.Checksums{}
		for _, algorithm := range []string{"sha512", "sha1"} {
			if err := checksums.Add(algorithm, file.Hashes[algorithm]); err != nil {
				return nil, err
			}
		}
		return &Resolved{
			Origin:    file.URL,
			Version:   v.VersionNumber,
			Checksums: checksums,
			Open:      r.http.HTTPOpener(file.URL, nil, nil),
		}, nil
	}
	filters := ""
	if len(query) > 0 {
		filters = fmt.Sprintf(" (loaders %v, game versions %v)", d.Loaders, d.GameVersions)
	}
	return nil, fmt.Errorf("modrinth: %s has no version matching %s%s", d.Project, wanted, filters)
}

// --- GitHub releases ---

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name               string `json:"name"`
		URL                string `json:"url"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Digest             string `json:"digest"`
	} `json:"assets"`
}

func (r *Resolver) github(ctx context.Context, d *template.Download) (*Resolved, error) {
	auth, err := r.httpAuth(d.Credentials)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(r.APIs.Github, "/") + "/repos/" + d.Repository
	headers := map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
	getJSON := func(u string, out any) error {
		response, err := r.http.Get(ctx, u, auth, headers)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		return json.NewDecoder(response.Body).Decode(out)
	}
	release := &githubRelease{}
	version := d.Version
	switch strings.ToLower(version) {
	case "", VersionLatest, VersionLatestRelease:
		err = getJSON(base+"/releases/latest", release)
	case VersionLatestPublished:
		// Pre-releases included: the newest release that is not a draft
		var releases []githubRelease
		if err = getJSON(base+"/releases?per_page=20", &releases); err == nil {
			release = nil
			for i := range releases {
				if !releases[i].Draft {
					release = &releases[i]
					break
				}
			}
			if release == nil {
				err = fmt.Errorf("github: %s has no published release", d.Repository)
			}
		}
	default:
		err = getJSON(base+"/releases/tags/"+url.PathEscape(version), release)
	}
	if err != nil {
		return nil, err
	}
	asset := regexp.MustCompile("^(?:" + d.Asset + ")$")
	var names []string
	for _, a := range release.Assets {
		names = append(names, a.Name)
		if !asset.MatchString(a.Name) {
			continue
		}
		checksums := fetch.Checksums{}
		if algorithm, digest, ok := strings.Cut(a.Digest, ":"); ok {
			if err = checksums.Add(algorithm, digest); err != nil {
				return nil, err
			}
		}
		res := &Resolved{Origin: a.BrowserDownloadURL, Version: release.TagName, Checksums: checksums}
		if auth != nil {
			// Assets of private repositories are only served through the API
			res.Open = r.http.HTTPOpener(a.URL, auth, map[string]string{"Accept": "application/octet-stream"})
		} else {
			res.Open = r.http.HTTPOpener(a.BrowserDownloadURL, nil, nil)
		}
		return res, nil
	}
	return nil, fmt.Errorf("github: release %s of %s has no asset matching %q (assets: %s)",
		release.TagName, d.Repository, d.Asset, strings.Join(names, ", "))
}

// --- LuckPerms ---

type luckPermsMetadata struct {
	Version   string            `json:"version"`
	Downloads map[string]string `json:"downloads"`
}

func (r *Resolver) luckPerms(ctx context.Context, d *template.Download) (*Resolved, error) {
	metadata := &luckPermsMetadata{}
	if err := r.http.GetJSON(ctx, r.APIs.LuckPerms, nil, metadata); err != nil {
		return nil, err
	}
	u, ok := metadata.Downloads[d.Platform]
	if !ok {
		platforms := make([]string, 0, len(metadata.Downloads))
		for platform := range metadata.Downloads {
			platforms = append(platforms, platform)
		}
		sort.Strings(platforms)
		return nil, fmt.Errorf("luckperms: unknown platform %q (%s)", d.Platform, strings.Join(platforms, ", "))
	}
	return &Resolved{Origin: u, Version: metadata.Version, Open: r.http.HTTPOpener(u, nil, nil)}, nil
}
