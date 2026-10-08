package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Published files. Every agent's published tree lives in one directory
// on the data volume, outside every agent's own tree:
//
//	data/public/<slug>/<path>
//
// Core is its only writer (artifact_publish, artifact_unpublish). The
// agent pod mounts public/<slug> read-only at /files/artifacts/public/
// and public/ itself read-only at /files/artifacts/shared/, so each
// peer, archived ones included, appears as shared/<peer>/ with no
// per-agent copy. In the agent's own tree the two are empty
// directories, kept only as mount points.

// PublishedDirName is the data-directory subtree holding every agent's
// published tree.
const PublishedDirName = "public"

// PublishedOwnDir and PublishedPeersDir are where an agent sees the
// published trees under /files/: its own, and everyone's by owner.
const (
	PublishedOwnDir   = "artifacts/public"
	PublishedPeersDir = "artifacts/shared"
)

// PublishedMountPoints are the directories of an agent's /files/ that
// the published trees are mounted over. Sync keeps each a real, empty
// directory.
var PublishedMountPoints = []string{PublishedOwnDir, PublishedPeersDir}

// PublishedRoot is data/public.
func PublishedRoot(dataDir string) string {
	return filepath.Join(dataDir, PublishedDirName)
}

// PublishedDir is data/public/<slug>.
func PublishedDir(dataDir, slug string) string {
	return filepath.Join(PublishedRoot(dataDir), slug)
}

// validSlug is the lexical check for a slug used as one path
// component.
func validSlug(slug string) bool {
	return slug != "" && slug != "." && slug != ".." && !strings.ContainsAny(slug, `/\`)
}

// EnsurePublishedDir makes data/public and data/public/<slug> real
// directories. The agent pod mounts both by subPath, and kubelet makes
// a missing one as root and refuses one with a symlink in it, so each
// agent's tree is ensured before its pod is created.
func EnsurePublishedDir(dataDir, slug string) error {
	if !validSlug(slug) {
		return fmt.Errorf("published dir: bad slug %q", slug)
	}
	return ensureRealDir(dataDir, PublishedDirName+"/"+slug)
}

// PublishedPathMaxBytes and PublishedNameMaxBytes bound a published
// path and each of its components.
const (
	PublishedPathMaxBytes = 1024
	PublishedNameMaxBytes = 255
)

// CleanPublishedPath validates p as a path under an owner's published
// tree and returns it cleaned and slash-separated. A leading
// /files/artifacts/public/ (the path the agent sees) is accepted and
// dropped. Refused: an empty path, an absolute one other than that,
// "..", a component starting with "." (the graph ignores those, so a
// dotfile would be published and never indexed), a backslash or NUL,
// and anything over the length limits.
func CleanPublishedPath(p string) (string, error) {
	orig := p
	p = strings.TrimSpace(p)
	for _, pre := range []string{ModelRootPath + "/" + PublishedOwnDir, PublishedOwnDir} {
		if rest, ok := strings.CutPrefix(p, pre); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			p = strings.TrimPrefix(rest, "/")
			break
		}
	}
	if p == "" {
		return "", fmt.Errorf("%q: name a path under your published area", orig)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%q: must be relative to /files/artifacts/public/", orig)
	}
	if strings.ContainsAny(p, "\\\x00") {
		return "", fmt.Errorf("%q: backslashes and NUL are not allowed", orig)
	}
	if len(p) > PublishedPathMaxBytes {
		return "", fmt.Errorf("%q: longer than %d bytes", orig, PublishedPathMaxBytes)
	}
	for _, part := range strings.Split(p, "/") {
		switch {
		case part == "":
			continue
		case part == "..":
			return "", fmt.Errorf("%q: .. is not allowed", orig)
		case strings.HasPrefix(part, "."):
			return "", fmt.Errorf("%q: a name starting with . is not published", orig)
		case len(part) > PublishedNameMaxBytes:
			return "", fmt.Errorf("%q: a component is longer than %d bytes", orig, PublishedNameMaxBytes)
		}
	}
	return path.Clean(p), nil
}

// PublishLimits bound one publish call.
type PublishLimits struct {
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxFiles      int
}

// PublishItem is one file a publish copies: Src relative to the source
// root it was planned against, Dest relative to the owner's published
// directory.
type PublishItem struct {
	Src  string
	Dest string
	Size int64
}

// PlanPublish lists what publishing src (a regular file or a directory
// beneath root) at dest takes, checking everything before anything is
// written. A directory is taken recursively. Only regular files are
// published: a symbolic link, FIFO, device or socket anywhere refuses
// the whole publish, naming it. Entries whose name starts with "." are
// skipped inside a directory (the graph ignores them) and returned in
// skipped; a src that names one itself is refused. dest "" places a
// directory's contents at the top of the published tree; a file needs
// a dest.
//
// root is opened by the caller on a directory core chose; src is the
// agent's path under it, walked without following a link at any
// component.
func PlanPublish(root *os.Root, src, dest string, lim PublishLimits) (items []PublishItem, skipped []string, err error) {
	parts, err := splitRel(src)
	if err != nil {
		return nil, nil, err
	}
	if len(parts) == 0 {
		return nil, nil, errors.New("name a file or directory, not the whole tree")
	}
	for _, p := range parts {
		if strings.HasPrefix(p, ".") {
			return nil, nil, fmt.Errorf("%s: a name starting with . is not published", src)
		}
	}
	parent, err := walkDirsNoFollow(root, parts[:len(parts)-1])
	if err != nil {
		return nil, nil, publishSourceErr(err)
	}
	if parent != root {
		defer func() { _ = parent.Close() }()
	}
	name := parts[len(parts)-1]
	shown := strings.Join(parts, "/")
	li, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, publishSourceErr(&PathError{Path: shown, Err: err})
	}
	var total int64
	add := func(srcRel, destRel string, size int64) error {
		if size > lim.MaxFileBytes {
			return fmt.Errorf("%s is %d bytes, over the %d-byte limit per file", srcRel, size, lim.MaxFileBytes)
		}
		total += size
		if total > lim.MaxTotalBytes {
			return fmt.Errorf("the files come to more than %d bytes, the limit per call; publish fewer at a time", lim.MaxTotalBytes)
		}
		if len(items) >= lim.MaxFiles {
			return fmt.Errorf("more than %d files, the limit per call; publish fewer at a time", lim.MaxFiles)
		}
		items = append(items, PublishItem{Src: srcRel, Dest: destRel, Size: size})
		return nil
	}
	switch {
	case li.Mode()&fs.ModeSymlink != 0:
		return nil, nil, fmt.Errorf("%s is a symbolic link; only regular files are published", shown)
	case li.Mode().IsRegular():
		if dest == "" {
			return nil, nil, errors.New("a file needs a destination path")
		}
		if err := add(shown, dest, li.Size()); err != nil {
			return nil, nil, err
		}
		return items, nil, nil
	case !li.IsDir():
		return nil, nil, fmt.Errorf("%s is not a regular file or directory", shown)
	}
	dir, err := openSubdirNoFollow(parent, name, shown)
	if err != nil {
		return nil, nil, publishSourceErr(err)
	}
	defer func() { _ = dir.Close() }()
	var walk func(d *os.Root, rel string) error
	walk = func(d *os.Root, rel string) error {
		entries, err := readDirIn(d, ".")
		if err != nil {
			return err
		}
		for _, e := range entries {
			childRel := e.Name()
			if rel != "" {
				childRel = rel + "/" + e.Name()
			}
			srcRel := shown + "/" + childRel
			if strings.HasPrefix(e.Name(), ".") {
				skipped = append(skipped, srcRel)
				continue
			}
			info, err := d.Lstat(e.Name())
			if err != nil {
				return err
			}
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				return fmt.Errorf("%s is a symbolic link; only regular files are published, so nothing was", srcRel)
			case info.Mode().IsRegular():
				if err := add(srcRel, path.Join(dest, childRel), info.Size()); err != nil {
					return err
				}
			case info.IsDir():
				sub, err := openSubdirNoFollow(d, e.Name(), srcRel)
				if err != nil {
					return publishSourceErr(err)
				}
				err = walk(sub, childRel)
				_ = sub.Close()
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("%s is not a regular file; only regular files are published, so nothing was", srcRel)
			}
		}
		return nil
	}
	if err := walk(dir, ""); err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return nil, skipped, fmt.Errorf("%s holds no file to publish", shown)
	}
	for i := range items {
		if items[i].Dest == "" || items[i].Dest == "." {
			return nil, nil, fmt.Errorf("%s: no destination path", items[i].Src)
		}
	}
	return items, skipped, nil
}

// publishSourceErr words a no-follow refusal for the agent.
func publishSourceErr(err error) error {
	var pe *PathError
	if errors.As(err, &pe) {
		switch {
		case errors.Is(err, ErrSymlink):
			return fmt.Errorf("%s is a symbolic link; only regular files are published", pe.Path)
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%s: not found", pe.Path)
		case errors.Is(err, ErrNotDir):
			return fmt.Errorf("%s: not a directory", pe.Path)
		}
	}
	return err
}

// CopyPublished writes each item from src into dst, each file
// atomically (a dot-named temp file beside it, then a rename), making
// directories as needed. A file already at an item's Dest is replaced.
// Each source is opened again without following a link, and the bytes
// copied are held to lim's per-file and per-call totals whatever the
// plan measured, so a file swapped or grown since PlanPublish cannot
// slip past either rule. Returns the Dest of every file written, in
// order, including when a later one fails.
func CopyPublished(src, dst *os.Root, items []PublishItem, lim PublishLimits) ([]string, error) {
	var written []string
	remaining := lim.MaxTotalBytes
	for _, it := range items {
		n, err := copyOnePublished(src, dst, it, lim.MaxFileBytes, remaining, lim.MaxTotalBytes)
		if err != nil {
			return written, err
		}
		remaining -= n
		written = append(written, it.Dest)
	}
	return written, nil
}

// copyOnePublished copies one item, holding it to maxFile bytes and to
// the remaining bytes of the call's maxTotal, and returns the bytes
// written.
func copyOnePublished(src, dst *os.Root, it PublishItem, maxFile, remaining, maxTotal int64) (int64, error) {
	limit := min(maxFile, remaining)
	n, err := copyOnePublishedAt(src, dst, it, limit)
	if errors.Is(err, errTooLarge) {
		if limit < maxFile {
			return 0, fmt.Errorf("the files grew past the %d-byte limit per call while they were read, at %s; publish fewer at a time", maxTotal, it.Src)
		}
		return 0, fmt.Errorf("%s grew past the %d-byte limit per file while it was read", it.Src, maxFile)
	}
	return n, err
}

func copyOnePublishedAt(src, dst *os.Root, it PublishItem, limit int64) (int64, error) {
	f, err := OpenNoFollowIn(src, it.Src)
	if err != nil {
		return 0, publishSourceErr(err)
	}
	defer func() { _ = f.Close() }()
	if d := path.Dir(it.Dest); d != "." {
		if err := mkdirAllIn(dst, d); err != nil {
			return 0, fmt.Errorf("publish %s: %s", it.Dest, blockedBy(err))
		}
	}
	if info, err := dst.Lstat(it.Dest); err == nil && info.IsDir() {
		return 0, fmt.Errorf("publish %s: a published directory has that name; unpublish it first", it.Dest)
	}
	n, err := writeReaderAtomicIn(dst, it.Dest, f, limit)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return 0, err
		}
		return 0, fmt.Errorf("publish %s: %w", it.Dest, err)
	}
	return n, nil
}

// blockedBy words a failure to make a published directory: almost
// always a published file where a directory of the same name is
// wanted.
func blockedBy(err error) string {
	if strings.Contains(err.Error(), "not a directory") {
		return "a published file is in the way of a directory of that path; unpublish it first"
	}
	return err.Error()
}

// RemovePublished removes rel, a file or a directory, beneath dst and
// returns the files it held (relative to dst, sorted). dst is core's
// own published tree; removal goes through it and never follows a
// link.
func RemovePublished(dst *os.Root, rel string) ([]string, error) {
	info, err := dst.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s is not published", rel)
		}
		return nil, err
	}
	var gone []string
	if info.IsDir() {
		sub, err := OpenDirNoFollow(dst.Name(), rel)
		if err != nil {
			return nil, err
		}
		werr := fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				gone = append(gone, path.Join(rel, p))
			}
			return nil
		})
		_ = sub.Close()
		if werr != nil {
			return nil, werr
		}
	} else {
		gone = append(gone, rel)
	}
	if err := removeAllIn(dst, rel); err != nil {
		return nil, err
	}
	sort.Strings(gone)
	return gone, nil
}

// EmptyPublished removes every published file of every owner, keeping
// data/public/ and each data/public/<slug>/ directory itself: a
// running pod mounts those by subPath, and a mount of a directory
// removed and made again would show the removed one. A restore empties
// the published trees first, so the archive's files are all that is
// published afterwards.
func EmptyPublished(dataDir string) error {
	pub, err := OpenDirNoFollow(dataDir, PublishedDirName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = pub.Close() }()
	owners, err := readDirIn(pub, ".")
	if err != nil {
		return err
	}
	for _, o := range owners {
		info, err := pub.Lstat(o.Name())
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if err := removeAllIn(pub, o.Name()); err != nil {
				return err
			}
			continue
		}
		if err := emptyDirIn(pub, o.Name()); err != nil {
			return err
		}
	}
	return nil
}

// emptyDirIn removes everything in the directory rel beneath root,
// leaving the directory itself.
func emptyDirIn(root *os.Root, rel string) error {
	entries, err := readDirIn(root, rel)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeAllIn(root, rel+"/"+e.Name()); err != nil {
			return err
		}
	}
	return nil
}
