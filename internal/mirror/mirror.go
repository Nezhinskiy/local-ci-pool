// Package mirror keeps a partial bare clone of a project and turns a commit's
// build inputs into a Docker build context. No credential is ever written or
// put in an argument: every git call gets the pool's token in its environment,
// and a credential helper given on the command line hands it to git.
package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/Nezhinskiy/local-ci-pool/internal/discovery"
)

const defaultBaseURL = "https://github.com"

// tokenEnv carries the token to the credential helper. It is set on the git
// process alone: not in a file, an argument or the pool's own environment.
const tokenEnv = "LOCAL_CI_POOL_GIT_TOKEN"

// credentialHelper answers git's "get" with the token in tokenEnv and ignores
// "store" and "erase", so git saves the token nowhere. It used to be
// `gh auth git-credential`, which reads the keychain on every git call: under
// launchd's background QoS on a loaded Mac that read times out, gh exits 1,
// and git, with prompts off, fails with "could not read Username".
const credentialHelper = `!f() { if test "$1" = get; then echo username=x-access-token; echo "password=$` + tokenEnv + `"; fi; }; f`

// Mirror is a directory of bare repositories, one per project.
type Mirror struct {
	Root string
	// BaseURL is the git host; the remote of a repository is
	// BaseURL/<owner>/<name>.git. Empty means https://github.com.
	BaseURL string
	// Token returns the token git authenticates to BaseURL with: in the pool,
	// the cached `gh auth token` of ghauth. Nil sends no credential.
	Token func(ctx context.Context) (string, error)
}

var (
	repoRE   = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	refRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	locks    sync.Map // dir -> *sync.Mutex
)

func checkRepo(repo string) error {
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") {
		return fmt.Errorf("invalid repository name %q", repo)
	}
	return nil
}

func checkRef(ref string) error {
	if !refRE.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") || strings.HasSuffix(ref, ".") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	return nil
}

func checkInputs(inputs []string) error {
	if len(inputs) == 0 {
		return errors.New("invalid inputs: none given")
	}
	for _, in := range inputs {
		if _, err := discovery.CleanRepoPath(in); err != nil {
			return fmt.Errorf("invalid input %q: %w", in, err)
		}
	}
	return nil
}

// Dir is the bare repository of a project: <Root>/<identity>.git, where the
// identity is the sanitised repository name, as in discovery.
func (m Mirror) Dir(repo string) string {
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:]
	}
	return filepath.Join(m.Root, discovery.Identity(name)+".git")
}

func (m Mirror) base() string {
	if m.BaseURL == "" {
		return defaultBaseURL
	}
	return strings.TrimRight(m.BaseURL, "/")
}

func (m Mirror) remoteURL(repo string) string {
	return m.base() + "/" + repo + ".git"
}

func lock(dir string) func() {
	v, _ := locks.LoadOrStore(dir, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// gitCmd builds a git call. Every call clears the configured credential
// helpers; with a Token it names the pool's helper for BaseURL alone and puts
// the token in the call's environment, so a lazy blob fetch during
// `git archive` authenticates the same way `git fetch` does.
func (m Mirror) gitCmd(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	global := []string{"-c", "credential.helper="}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "LC_ALL=C")
	if m.Token != nil {
		tok, err := m.Token(ctx)
		if err != nil {
			return nil, err
		}
		global = append(global, "-c", "credential."+m.base()+".helper="+credentialHelper)
		env = append(env, tokenEnv+"="+tok)
	}
	// --literal-pathspecs: an input is a path, never a glob or pathspec magic,
	// for ls-tree and archive alike.
	global = append(global, "--literal-pathspecs")
	cmd := exec.CommandContext(ctx, "git", append(global, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd, nil
}

func (m Mirror) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd, err := m.gitCmd(ctx, dir, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", firstWord(args), err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", firstWord(args), err, tail(stderr.String()))
	}
	return stdout.String(), nil
}

func firstWord(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = "..." + s[len(s)-500:]
	}
	return s
}

// Fetch brings branch ref of repo ("owner/name") into the project's bare
// repository, without file contents (blobs arrive when something needs them),
// and returns the commit the branch points at. ref is a branch name, fetched
// as refs/heads/<ref>: git would resolve a bare name to a tag of the same
// name first, and anyone who can push a tag would then decide what is built.
func (m Mirror) Fetch(ctx context.Context, repo, ref string) (string, error) {
	if err := checkRepo(repo); err != nil {
		return "", err
	}
	if err := checkRef(ref); err != nil {
		return "", err
	}
	dir := m.Dir(repo)
	defer lock(dir)()

	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(m.Root, 0o700); err != nil {
			return "", err
		}
		if _, err := m.run(ctx, m.Root, "init", "--bare", "-q", dir); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if _, err := m.run(ctx, dir, "config", "remote.origin.url", m.remoteURL(repo)); err != nil {
		return "", err
	}
	branch := "refs/heads/" + strings.TrimPrefix(ref, "refs/heads/")
	if _, err := m.run(ctx, dir, "fetch", "--filter=blob:none", "--no-tags", "origin", "+"+branch+":"+branch); err != nil {
		return "", err
	}
	out, err := m.run(ctx, dir, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if !commitRE.MatchString(commit) {
		return "", fmt.Errorf("git rev-parse printed %q, not a commit", commit)
	}
	return commit, nil
}

// InputsDigest is the first 12 hex of sha256 over `git ls-tree -r --full-tree`
// of the inputs at commit. The listing holds the blob SHAs, so the digest
// follows the content of the inputs and nothing else: a commit that touches
// other files keeps it.
func (m Mirror) InputsDigest(ctx context.Context, dir, commit string, inputs []string) (string, error) {
	if !commitRE.MatchString(commit) {
		return "", fmt.Errorf("invalid commit %q", commit)
	}
	if err := checkInputs(inputs); err != nil {
		return "", err
	}
	out, err := m.run(ctx, dir, append([]string{"ls-tree", "-r", "--full-tree", commit, "--"}, inputs...)...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("the inputs %q match nothing in commit %s", inputs, commit)
	}
	sum := sha256.Sum256([]byte(out))
	return hex.EncodeToString(sum[:])[:12], nil
}

type archive struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr *bytes.Buffer
	done   bool
	err    error
}

// Archive streams `git archive --format=tar commit -- inputs`, which keeps file
// modes and symlinks. A git failure shows up as the error of the read that
// reaches the end of the stream.
func (m Mirror) Archive(ctx context.Context, dir, commit string, inputs []string) (io.ReadCloser, error) {
	if !commitRE.MatchString(commit) {
		return nil, fmt.Errorf("invalid commit %q", commit)
	}
	if err := checkInputs(inputs); err != nil {
		return nil, err
	}
	// tar.umask is pinned so the modes (0755 and 0644) do not depend on a git
	// config of the machine: the default umask 002 would make them 0775 and 0664.
	cmd, err := m.gitCmd(ctx, dir, append([]string{"-c", "tar.umask=022", "archive", "--format=tar", commit, "--"}, inputs...)...)
	if err != nil {
		return nil, fmt.Errorf("git archive: %w", err)
	}
	a := &archive{cmd: cmd, stderr: &bytes.Buffer{}}
	cmd.Stderr = a.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	a.out = out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git archive: %w", err)
	}
	return a, nil
}

func (a *archive) wait() error {
	if !a.done {
		a.done = true
		if err := a.cmd.Wait(); err != nil {
			a.err = fmt.Errorf("git archive: %w: %s", err, tail(a.stderr.String()))
		}
	}
	return a.err
}

func (a *archive) Read(p []byte) (int, error) {
	n, err := a.out.Read(p)
	if errors.Is(err, io.EOF) {
		if werr := a.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// Close stops git if the stream was not read to the end.
func (a *archive) Close() error {
	if !a.done {
		if a.cmd.Process != nil {
			_ = a.cmd.Process.Kill()
		}
		a.done = true
		_ = a.cmd.Wait()
	}
	return nil
}
