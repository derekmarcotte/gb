package vendor

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/constabulary/gb"
)

// RemoteRepo describes a remote dvcs repository.
type RemoteRepo interface {

	// Checkout checks out a specific branch, tag, or revision.
	// The interpretation of these three values is impementation
	// specific.
	Checkout(branch, tag, revision string) (WorkingCopy, error)

	// URL returns the URL the clone was taken from. It should
	// only be called after Clone.
	URL() string
}

// WorkingCopy represents a local copy of a remote dvcs repository.
type WorkingCopy interface {

	// Dir is the root of this working copy.
	Dir() string

	// Revision returns the revision of this working copy.
	Revision() (string, error)

	// Branch returns the branch to which this working copy belongs.
	Branch() (string, error)

	// Destroy removes the working copy and cleans path to the working copy.
	Destroy() error
}

var (
	ghregex = regexp.MustCompile(`^(?P<root>github\.com/([A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+))(/[A-Za-z0-9_.\-]+)*$`)
	bbregex = regexp.MustCompile(`^(?P<root>bitbucket\.org/(?P<bitname>[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+))(/[A-Za-z0-9_.\-]+)*$`)
	lpregex = regexp.MustCompile(`^launchpad.net/([A-Za-z0-9-._]+)(/[A-Za-z0-9-._]+)?(/.+)?`)
	gcregex = regexp.MustCompile(`^(?P<root>code\.google\.com/[pr]/(?P<project>[a-z0-9\-]+)(\.(?P<subrepo>[a-z0-9\-]+))?)(/[A-Za-z0-9_.\-]+)*$`)
	genre   = regexp.MustCompile(`^(?P<root>(?P<repo>([a-z0-9.\-]+\.)+[a-z0-9.\-]+(:[0-9]+)?/[A-Za-z0-9_.\-/]*?)\.(?P<vcs>bzr|git|hg|svn))(/[A-Za-z0-9_.\-]+)*$`)
)

// DeduceRemoteRepo takes a potential import path and returns a RemoteRepo
// representing the remote location of the source of an import path.
// If deduction would cause traversal of an insecure host, a message will be
// printed and the travelsal path will be ignored.
func DeduceRemoteRepo(path string, insecure bool) (RemoteRepo, string, error) {
	validimport := regexp.MustCompile(`^([A-Za-z0-9-]+)(.[A-Za-z0-9-]+)+(/[A-Za-z0-9-_.]+)+$`)
	if !validimport.MatchString(path) {
		return nil, "", fmt.Errorf("%q is not a valid import path", path)
	}

	switch {
	case ghregex.MatchString(path):
		v := ghregex.FindStringSubmatch(path)
		repo, err := Gitrepo("github.com", v[2], insecure)
		return repo, v[0][len(v[1]):], err
	case bbregex.MatchString(path):
		v := bbregex.FindStringSubmatch(path)
		repo, err := Gitrepo("bitbucket.org", v[2], insecure)
		if err == nil {
			return repo, v[0][len(v[1]):], nil
		}
		repo, err = Hgrepo("bitbucket.org", v[2], insecure)
		if err == nil {
			return repo, v[0][len(v[1]):], nil
		}
		return nil, "", fmt.Errorf("unknown repository type")
	case gcregex.MatchString(path):
		v := gcregex.FindStringSubmatch(path)
		repo, err := Hgrepo("code.google.com", "p/"+v[2], insecure)
		if err == nil {
			return repo, v[5], nil
		}
		repo, err = Gitrepo("code.google.com", "p/"+v[2], insecure)
		if err == nil {
			return repo, v[5], nil
		}
		return nil, "", fmt.Errorf("unknown repository type")
	case lpregex.MatchString(path):
		v := lpregex.FindStringSubmatch(path)
		v = append(v, "", "")
		if v[2] == "" {
			// launchpad.net/project"
			repo, err := Bzrrepo(fmt.Sprintf("https://launchpad.net/%v", v[1]))
			return repo, "", err
		}
		// launchpad.net/project/series"
		repo, err := Bzrrepo(fmt.Sprintf("https://launchpad.net/%s/%s", v[1], v[2]))
		return repo, v[3], err
	}

	// try the general syntax
	if genre.MatchString(path) {
		v := genre.FindStringSubmatch(path)
		switch v[5] {
		case "git":
			x := strings.SplitN(v[1], "/", 2)
			repo, err := Gitrepo(x[0], x[1], insecure)
			return repo, v[6], err
		case "hg":
			x := strings.SplitN(v[1], "/", 2)
			repo, err := Hgrepo(x[0], x[1], insecure)
			return repo, v[6], err
		case "bzr":
			repo, err := Bzrrepo("https://" + v[1])
			return repo, v[6], err
		default:
			return nil, "", fmt.Errorf("unknown repository type: %q", v[5])

		}
	}

	// no idea, try to resolve as a vanity import
	importpath, vcs, reporoot, err := ParseMetadata(path, insecure)
	if err != nil {
		return nil, "", err
	}
	u, err := url.Parse(reporoot)
	if err != nil {
		return nil, "", err
	}
	extra := path[len(importpath):]
	switch vcs {
	case "git":
		repo, err := Gitrepo(u.Host, u.Path[1:], insecure, u.Scheme)
		return repo, extra, err
	case "hg":
		repo, err := Hgrepo(u.Host, u.Path[1:], insecure, u.Scheme)
		return repo, extra, err
	case "bzr":
		repo, err := Bzrrepo(reporoot)
		return repo, extra, err
	default:
		return nil, "", fmt.Errorf("unknown repository type: %q", vcs)
	}
}

// Gitrepo returns a RemoteRepo representing a remote git repository.
func Gitrepo(host, path string, insecure bool, schemes ...string) (RemoteRepo, error) {
	if schemes == nil {
		schemes = []string{"https", "git", "ssh", "http"}
	}
	url, err := probeGitUrl(schemes, host, path, insecure)
	if err != nil {
		return nil, err
	}
	return &gitrepo{
		url: url,
	}, nil
}

func probeGitUrl(schemes []string, host, path string, insecure bool) (string, error) {
	for _, scheme := range schemes {
		switch scheme {
		case "https", "ssh":
			url := scheme + "://" + host + "/" + path
			out, err := run("git", "ls-remote", url, "HEAD")
			if err == nil && bytes.Contains(out, []byte("HEAD")) {
				return url, nil
			}
		case "http", "git":
			url := scheme + "://" + host + "/" + path
			if !insecure {
				gb.Infof("skipping insecure protocol: %v", url)
			} else {
				out, err := run("git", "ls-remote", url, "HEAD")
				if err == nil && bytes.Contains(out, []byte("HEAD")) {
					return url, nil
				}
			}
		}
	}
	return "", fmt.Errorf("unable to determine remote protocol")
}

// gitrepo is a git RemoteRepo.
type gitrepo struct {

	// remote repository url, see man 1 git-clone
	url string
}

func (g *gitrepo) URL() string {
	return g.url
}

// Checkout fetchs the remote branch, tag, or revision. If more than one is
// supplied, an error is returned. If the branch is blank,
// then the default remote branch will be used. If the branch is "HEAD", an
// error will be returned.
func (g *gitrepo) Checkout(branch, tag, revision string) (WorkingCopy, error) {
	if branch == "HEAD" {
		return nil, fmt.Errorf("cannot update %q as it has been previously fetched with -tag or -revision. Please use gb vendor delete then fetch again.", g.url)
	}
	if !atMostOne(branch, tag, revision) {
		return nil, fmt.Errorf("only one of branch, tag or revision may be supplied")
	}
	dir, err := mktmp()
	if err != nil {
		return nil, err
	}
	wc := workingcopy{
		path: dir,
	}

	args := []string{
		"clone",
		"-q", // silence progress report to stderr
		g.url,
		dir,
	}
	if branch != "" {
		args = append(args, "--branch", branch)
	}

	if _, err := run("git", args...); err != nil {
		wc.Destroy()
		return nil, err
	}

	if revision != "" || tag != "" {
		if err := runOutPath(os.Stderr, dir, "git", "checkout", "-q", oneOf(revision, tag)); err != nil {
			wc.Destroy()
			return nil, err
		}
	}

	return &GitClone{wc}, nil
}

type workingcopy struct {
	path string
}

func (w workingcopy) Dir() string { return w.path }

func (w workingcopy) Destroy() error {
	if err := RemoveAll(w.path); err != nil {
		return err
	}
	parent := filepath.Dir(w.path)
	return cleanPath(parent)
}

// GitClone is a git WorkingCopy.
type GitClone struct {
	workingcopy
}

func (g *GitClone) Revision() (string, error) {
	rev, err := runPath(g.path, "git", "rev-parse", "HEAD")
	return strings.TrimSpace(string(rev)), err
}

func (g *GitClone) Branch() (string, error) {
	rev, err := runPath(g.path, "git", "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(string(rev)), err
}

// Hgrepo returns a RemoteRepo representing a remote git repository.
func Hgrepo(host, path string, insecure bool, schemes ...string) (RemoteRepo, error) {
	if schemes == nil {
		schemes = []string{"https", "http"}
	}
	url, err := probeHgUrl(schemes, host, path, insecure)
	if err != nil {
		return nil, err
	}
	return &hgrepo{
		url: url,
	}, nil
}

func probeHgUrl(schemes []string, host, path string, insecure bool) (string, error) {
	for _, scheme := range schemes {
		switch scheme {
		case "https":
			url := scheme + "://" + host + "/" + path
			if _, err := run("hg", "identify", url); err == nil {
				return url, nil
			}
		case "http", "git":
			url := scheme + "://" + host + "/" + path
			if !insecure {
				gb.Infof("skipping insecure protocol: %v", url)
			} else {
				if _, err := run("hg", "identify", url); err == nil {
					return url, nil
				}
			}
		}
	}
	return "", fmt.Errorf("unable to determine remote protocol")
}

// hgrepo is a Mercurial repo.
type hgrepo struct {

	// remote repository url, see man 1 hg
	url string
}

func (h *hgrepo) URL() string { return h.url }

func (h *hgrepo) Checkout(branch, tag, revision string) (WorkingCopy, error) {
	if !atMostOne(tag, revision) {
		return nil, fmt.Errorf("only one of tag or revision may be supplied")
	}
	dir, err := mktmp()
	if err != nil {
		return nil, err
	}
	args := []string{
		"clone",
		h.url,
		dir,
	}

	if branch != "" {
		args = append(args, "--branch", branch)
	}
	if err := runOut(os.Stderr, "hg", args...); err != nil {
		RemoveAll(dir)
		return nil, err
	}
	if revision != "" {
		if err := runOut(os.Stderr, "hg", "--cwd", dir, "update", "-r", revision); err != nil {
			RemoveAll(dir)
			return nil, err
		}
	}

	return &HgClone{
		workingcopy{
			path: dir,
		},
	}, nil
}

// HgClone is a mercurial WorkingCopy.
type HgClone struct {
	workingcopy
}

func (h *HgClone) Revision() (string, error) {
	rev, err := run("hg", "--cwd", h.path, "id", "-i")
	return strings.TrimSpace(string(rev)), err
}

func (h *HgClone) Branch() (string, error) {
	rev, err := run("hg", "--cwd", h.path, "branch")
	return strings.TrimSpace(string(rev)), err
}

// Bzrrepo returns a RemoteRepo representing a remote bzr repository.
func Bzrrepo(url string) (RemoteRepo, error) {
	if err := probeBzrUrl(url); err != nil {
		return nil, err
	}
	return &bzrrepo{
		url: url,
	}, nil
}

func probeBzrUrl(url string) error {
	_, err := run("bzr", "info", url)
	return err
}

// bzrrepo is a bzr RemoteRepo.
type bzrrepo struct {

	// remote repository url
	url string
}

func (b *bzrrepo) URL() string {
	return b.url
}

func (b *bzrrepo) Checkout(branch, tag, revision string) (WorkingCopy, error) {
	if !atMostOne(tag, revision) {
		return nil, fmt.Errorf("only one of tag or revision may be supplied")
	}
	dir, err := mktmp()
	if err != nil {
		return nil, err
	}
	wc := filepath.Join(dir, "wc")
	if err := runOut(os.Stderr, "bzr", "branch", b.url, wc); err != nil {
		RemoveAll(dir)
		return nil, err
	}

	return &BzrClone{
		workingcopy{
			path: dir,
		},
	}, nil
}

// BzrClone is a bazaar WorkingCopy.
type BzrClone struct {
	workingcopy
}

func (b *BzrClone) Revision() (string, error) {
	return "1", nil
}

func (b *BzrClone) Branch() (string, error) {
	return "master", nil
}

func cleanPath(path string) error {
	if files, _ := ioutil.ReadDir(path); len(files) > 0 || filepath.Base(path) == "src" {
		return nil
	}
	parent := filepath.Dir(path)
	if err := RemoveAll(path); err != nil {
		return err
	}
	return cleanPath(parent)
}

func mkdir(path string) error {
	return os.MkdirAll(path, 0755)
}

func mktmp() (string, error) {
	return ioutil.TempDir("", "gb-vendor-")
}

func run(c string, args ...string) ([]byte, error) {
	var buf bytes.Buffer
	err := runOut(&buf, c, args...)
	return buf.Bytes(), err
}

func runOut(w io.Writer, c string, args ...string) error {
	cmd := exec.Command(c, args...)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runPath(path string, c string, args ...string) ([]byte, error) {
	var buf bytes.Buffer
	err := runOutPath(&buf, path, c, args...)
	return buf.Bytes(), err
}

func runOutPath(w io.Writer, path string, c string, args ...string) error {
	cmd := exec.Command(c, args...)
	cmd.Dir = path
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// atMostOne returns true if no more than one string supplied is not empty.
func atMostOne(args ...string) bool {
	var c int
	for _, arg := range args {
		if arg != "" {
			c++
		}
	}
	return c < 2
}

// oneof returns the first non empty string
func oneOf(args ...string) string {
	for _, arg := range args {
		if arg != "" {
			return arg
		}
	}
	return ""
}
