package daemon

import (
	"net/url"
	"path"
	"strings"
)

// matchProject returns p when repository names it, or nil. A spelling names
// a project entry when it is:
//
//   - the entry's repository exactly as configured, or the entry's ID (the
//     orchestrator may send the project slug as the repository, and the slug
//     need not resemble the repository name);
//   - the same repository location (sameRepositoryLocation), whatever its
//     scheme, user, ".git" suffix or letter case;
//   - a bare repository name (one path segment, no host) equal to the last
//     segment of the configured repository. It carries no host and no owner,
//     so it cannot name a repository anywhere else.
//
// A spelling that carries a host or a path never matches by suffix: an
// earlier suffix match admitted "https://elsewhere.example/x/github.com/o/r"
// for "github.com/o/r", and the runner clones the spelling it was given.
func matchProject(p *ProjectConfig, repository string) *ProjectConfig {
	if p.Repository == repository || p.ID == repository {
		return p
	}
	if sameRepositoryLocation(p.Repository, repository) {
		return p
	}
	if name, ok := bareRepositoryName(repository); ok {
		if configured, ok := parseRepositoryLocation(p.Repository); ok && !configured.local && lastPathSegment(configured.path) == name {
			return p
		}
	}
	return nil
}

// repositoryLocation is a repository spelling reduced to what names the
// repository: the host (empty when the spelling carries none) and the path
// without slashes at either end or a ".git" suffix, both lower-cased. A
// local repository (an absolute or relative filesystem path, or a file URL)
// keeps its cleaned path as written and only ever equals another local one.
type repositoryLocation struct {
	host  string
	path  string
	local bool
}

// parseRepositoryLocation reduces a repository spelling: an https, http, ssh
// or git URL; an scp-style "[user@]host:owner/name"; a scheme-less
// "host/owner/name" whose first segment looks like a host name; a host-less
// "owner/name"; or a local path. A spelling it cannot reduce, or one that
// carries a query or a fragment, is not a location (ok is false), so it
// matches nothing.
func parseRepositoryLocation(raw string) (repositoryLocation, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, " \t\r\n\\") {
		return repositoryLocation{}, false
	}
	var loc repositoryLocation
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return repositoryLocation{}, false
		}
		if strings.EqualFold(u.Scheme, "file") {
			if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
				return repositoryLocation{}, false
			}
			loc = repositoryLocation{path: u.Path, local: true}
			break
		}
		if u.Host == "" {
			return repositoryLocation{}, false
		}
		loc = repositoryLocation{host: u.Host, path: u.Path}
	case strings.ContainsAny(s, "?#"):
		return repositoryLocation{}, false
	case strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../"):
		loc = repositoryLocation{path: s, local: true}
	default:
		if host, rest, ok := strings.Cut(s, ":"); ok && !strings.Contains(host, "/") {
			// scp-style [user@]host:owner/name
			if at := strings.LastIndex(host, "@"); at >= 0 {
				host = host[at+1:]
			}
			if host == "" {
				return repositoryLocation{}, false
			}
			loc = repositoryLocation{host: host, path: rest}
			break
		}
		first, rest, _ := strings.Cut(s, "/")
		if rest != "" && (strings.Contains(first, ".") || strings.EqualFold(first, "localhost")) {
			loc = repositoryLocation{host: first, path: rest}
			break
		}
		loc = repositoryLocation{path: s}
	}
	if loc.local {
		loc.path = strings.TrimSuffix(path.Clean(loc.path), ".git")
		return loc, loc.path != "" && loc.path != "."
	}
	loc.host = strings.ToLower(loc.host)
	loc.path = strings.ToLower(strings.Trim(strings.TrimSuffix(strings.Trim(loc.path, "/"), ".git"), "/"))
	if loc.path == "" {
		return repositoryLocation{}, false
	}
	for _, segment := range strings.Split(loc.path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return repositoryLocation{}, false
		}
	}
	return loc, true
}

// sameRepositoryLocation reports whether incoming names the repository
// configured as configured. Two hosted spellings must agree on host and
// path. A host-less "owner/name" stands for the configured repository's own
// host when it is the incoming side; when it is the configured side, it
// means GitHub (the convention the local runtime expands it with), so an
// incoming spelling on any other host does not match it.
func sameRepositoryLocation(configured, incoming string) bool {
	c, ok := parseRepositoryLocation(configured)
	if !ok {
		return false
	}
	i, ok := parseRepositoryLocation(incoming)
	if !ok {
		return false
	}
	if c.local || i.local {
		return c.local && i.local && c.path == i.path
	}
	if c.path != i.path {
		return false
	}
	switch {
	case c.host != "" && i.host != "":
		return c.host == i.host
	case c.host == "" && i.host == "":
		return true
	case i.host == "":
		return isOwnerNamePath(i.path)
	default:
		return i.host == "github.com" && isOwnerNamePath(c.path)
	}
}

// bareRepositoryName returns the lower-cased name when repository is a single
// path segment with no host, scheme or separator.
func bareRepositoryName(repository string) (string, bool) {
	loc, ok := parseRepositoryLocation(repository)
	if !ok || loc.local || loc.host != "" || strings.Contains(loc.path, "/") {
		return "", false
	}
	return loc.path, true
}

func isOwnerNamePath(p string) bool {
	return strings.Count(p, "/") == 1
}

func lastPathSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
