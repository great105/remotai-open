package web

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Project represents a repository found on disk (any folder containing .git).
type project struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Parent   string `json:"parent"`
	Modified int64  `json:"modified"` // unix ms, by .git/HEAD mtime
}

var (
	projectsCache struct {
		sync.Mutex
		list  []project
		at    time.Time
		valid time.Duration
	}
)

const (
	projectsCacheTTL  = 60 * time.Second
	projectsScanDepth = 4
	projectsScanMax   = 300 // cap results to avoid huge payloads
)

// apiProjects returns a list of folders containing `.git`, scanned under
// common user directories. Results are cached for 60s.
func (s *Server) apiProjects(w http.ResponseWriter, r *http.Request, uid int64) {
	projectsCache.Lock()
	if time.Since(projectsCache.at) < projectsCacheTTL && projectsCache.list != nil {
		out := projectsCache.list
		projectsCache.Unlock()
		jsonResp(w, map[string]any{"projects": out, "cached": true})
		return
	}
	projectsCache.Unlock()

	roots := projectRoots()
	var found []project
	seen := make(map[string]bool)

	for _, root := range roots {
		scanForGit(root, projectsScanDepth, seen, &found)
		if len(found) >= projectsScanMax {
			break
		}
	}

	// Sort by most recently modified (.git/HEAD) first.
	sort.Slice(found, func(i, j int) bool { return found[i].Modified > found[j].Modified })
	if len(found) > projectsScanMax {
		found = found[:projectsScanMax]
	}

	projectsCache.Lock()
	projectsCache.list = found
	projectsCache.at = time.Now()
	projectsCache.Unlock()

	jsonResp(w, map[string]any{"projects": found, "cached": false})
}

// projectRoots returns directories to scan for projects.
// Includes home + Desktop/Documents/Downloads/projects for each user on Windows.
func projectRoots() []string {
	var roots []string
	seen := make(map[string]bool)

	addRoot := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			seen[p] = true
			roots = append(roots, p)
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		addRoot(home)
		for _, sub := range []string{"Desktop", "Documents", "Downloads", "projects", "Projects", "src", "code", "dev", "Work"} {
			addRoot(filepath.Join(home, sub))
		}
	}

	// On Windows, os.UserHomeDir() under LocalSystem service points to
	// systemprofile — enumerate actual user profiles under C:\Users.
	if runtime.GOOS == "windows" {
		if entries, err := os.ReadDir(`C:\Users`); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				name := entry.Name()
				switch name {
				case "Public", "Default", "Default User", "All Users":
					continue
				}
				userDir := filepath.Join(`C:\Users`, name)
				for _, sub := range []string{"Desktop", "Documents", "Downloads", "projects", "Projects", "src", "code", "dev", "Work"} {
					addRoot(filepath.Join(userDir, sub))
				}
			}
		}
	} else {
		// Headless Linux projects commonly live outside the service user's home.
		// Scan /home at shallow project depth plus deployment roots explicitly.
		addRoot("/home")
		addRoot("/opt")
		addRoot("/srv")
		addRoot("/var/www")
	}

	return roots
}

// scanForGit walks root up to maxDepth, collecting directories that contain .git.
func scanForGit(root string, maxDepth int, seen map[string]bool, out *[]project) {
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > maxDepth || len(*out) >= projectsScanMax {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}

		for _, e := range entries {
			name := e.Name()
			// Skip noise early.
			if name[0] == '.' && name != ".git" {
				// Hidden dirs (.vscode, .idea, .cache) generally uninteresting
				// but allow .git as a marker.
				if name != ".local" {
					continue
				}
			}
			switch name {
			case "node_modules", "vendor", "target", "build", "dist",
				"__pycache__", "bin", "obj", ".next", "AppData":
				continue
			}

			if !e.IsDir() {
				continue
			}

			full := filepath.Join(dir, name)

			if name == ".git" {
				if seen[dir] {
					continue
				}
				seen[dir] = true
				p := project{
					Name:   filepath.Base(dir),
					Path:   dir,
					Parent: filepath.Base(filepath.Dir(dir)),
				}
				if info, err := os.Stat(filepath.Join(full, "HEAD")); err == nil {
					p.Modified = info.ModTime().UnixMilli()
				} else if info, err := os.Stat(full); err == nil {
					p.Modified = info.ModTime().UnixMilli()
				}
				*out = append(*out, p)
				// Don't descend into the project itself.
				return
			}
			walk(full, depth+1)
			if len(*out) >= projectsScanMax {
				return
			}
		}
	}
	walk(root, 0)
}
