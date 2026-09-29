package main

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var backendLabelWritten bool

const (
	commandName        = "git-cow-worktree"
	maxOtherCandidates = 8
	// How far down the ranking to look for a source worth cloning from.
	// Each rejection costs a pair of Git invocations against a worktree
	// that turned out to be useless, and a source this far from the top is
	// unlikely to be worth much anyway.
	maxSourceAttempts = 3
)

// Options collected from argv after light parsing.
type cowMode string

const (
	cowModeLegacy cowMode = "" // preserve the tool's existing behavior
	cowModeAuto   cowMode = "auto"
	cowModeOff    cowMode = "off"
)

type Options struct {
	CowMode       cowMode
	Verbose       bool
	FromSpecified bool
	FromPath      string   // --from <path>; empty means auto-pick
	NoCheckout    bool     // final --no-checkout state (force passthrough)
	Orphan        bool     // final --orphan state (force passthrough)
	Forward       []string // flags+args forwarded to our internal `git worktree add`
	GitArgs       []string // sanitized args forwarded to passthrough `git worktree add`
	UseSeparator  bool     // whether positional args were introduced by --
	TargetPath    string   // positional <path>
	CommitIsh     string   // positional <commit-ish>, may be empty
}

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "--summary":
		fmt.Println("Create a Git worktree using stock or copy-on-write checkout")
		return
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return
	case "add":
	default:
		printUsage(os.Stderr)
		os.Exit(2)
	}

	participating := os.Getenv("SQ_INVOKED_BY_SQ") != ""
	mode := cowModeLegacy
	if participating {
		opts, err := parseArgs(os.Args[2:])
		if err != nil {
			exitWithError(err, true)
		}
		if opts.CowMode != cowModeLegacy {
			exitWithError(errors.New("--cow is controlled by the beta experiment"), true)
		}
		mode, err = assignedCowMode(os.Args[2:])
		if err != nil {
			exitWithError(err, true)
		}
		writeMetricsLabels("arm:" + experimentArm(mode))
	}
	if err := runWithMode(os.Args[2:], mode); err != nil {
		exitWithError(err, participating)
	}
	if participating {
		writeMetricsLabels("result:success")
	}
}

func exitWithError(err error, participating bool) {
	fmt.Fprintln(os.Stderr, commandName+":", err)
	if participating {
		if !backendLabelWritten {
			writeMetricsLabels("backend:unknown")
		}
		writeMetricsLabels("result:failure")
	}
	os.Exit(1)
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, `USAGE
   sq git-cow-worktree add [git-worktree-add options] <path> [<commit-ish>]
   git cow-worktree add [git-worktree-add options] <path> [<commit-ish>]

DESCRIPTION
   Create a Git worktree using the beta's stable 50:50 stock/CoW assignment.
   Repeating the same repository, target path, and arguments keeps the same arm.

OPTIONS
   --from <path>   Select a source worktree for the CoW arm
   -v, --verbose   Show backend, source, clone counts, and timings

SUPPORT
   https://github.com/josharian/git-cow-worktree`)
}

func runWithMode(args []string, mode cowMode) error {
	if mode != cowModeLegacy {
		args = append([]string{"--cow=" + string(mode)}, args...)
	}
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}
	if mode != cowModeLegacy && opts.Verbose {
		fmt.Fprintf(os.Stderr, "%s: experiment backend=%s\n", commandName, mode)
	}
	return run(args)
}

func assignedCowMode(args []string) (cowMode, error) {
	repoDir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return assignedCowModeInDir(args, repoDir)
}

func assignedCowModeInDir(args []string, repoDir string) (cowMode, error) {
	opts, err := parseArgs(args)
	if err != nil {
		return "", err
	}
	if opts.TargetPath == "" {
		return "", errors.New("missing <path> argument")
	}
	commonDir := repoDir
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = repoDir
	if out, gitErr := cmd.Output(); gitErr == nil {
		commonDir = strings.TrimSpace(string(out))
	}
	targetAbs, err := osAbs(opts.TargetPath)
	if err != nil {
		return "", err
	}
	key := strings.Join([]string{commonDir, targetAbs, strings.Join(opts.GitArgs, "\x00")}, "\x00")
	sum := sha256.Sum256([]byte(key))
	if sum[0]&1 == 0 {
		return cowModeAuto, nil
	}
	return cowModeOff, nil
}

func experimentArm(mode cowMode) string {
	if mode == cowModeOff {
		return "stock"
	}
	return "cow"
}

func writeMetricsLabels(labels ...string) {
	path := os.Getenv("SQ_METRICS_PIPE")
	if path == "" || len(labels) == 0 {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "LABELS %s\n", strings.Join(labels, " "))
}

func run(args []string) (retErr error) {
	opts, err := parseArgs(args)
	if err != nil {
		return err
	}

	// Explicitly disabled means exactly stock Git, with no registration or
	// filesystem work performed by this process.
	if opts.CowMode == cowModeOff {
		writeBackendLabel("stock")
		return execGitWorktreeAdd(opts.GitArgs)
	}

	// --no-checkout and --orphan cannot use our seed-then-checkout flow.
	if opts.NoCheckout || opts.Orphan {
		writeBackendLabel("stock")
		return execGitWorktreeAdd(opts.GitArgs)
	}
	if opts.TargetPath == "" {
		return errors.New("missing <path> argument")
	}

	repoDir, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := resolveRev(repoDir, "HEAD"); err != nil {
		// Git may infer orphan creation in an unborn repository, which is
		// incompatible with the optimized --no-checkout staging flow.
		writeBackendLabel("stock")
		return execGitWorktreeAdd(opts.GitArgs)
	}
	targetAbs, err := osAbs(opts.TargetPath)
	if err != nil {
		return err
	}

	var explicitSource *Worktree
	if opts.FromSpecified {
		wt, err := resolveFromPath(opts.FromPath)
		if err != nil {
			return err
		}
		explicitSource = &wt
	}

	overallStart := time.Now()
	phase := func(name string, t0 time.Time) {
		if opts.Verbose {
			fmt.Fprintf(os.Stderr, "%s: %-20s %v\n", commandName, name, time.Since(t0).Round(time.Millisecond))
		}
	}

	guessRev := cmp.Or(opts.CommitIsh, "HEAD")
	var a *analysis
	// Automatic mode is deliberately opt-in. It does all capability checks
	// before registering the worktree; an unsupported filesystem or an
	// unusable source therefore takes the exact stock-Git path.
	if opts.CowMode == cowModeAuto {
		t := time.Now()
		sha, err := resolveRev(repoDir, guessRev)
		if err != nil {
			return fallbackToStock(opts, fmt.Errorf("resolve target: %w", err))
		}
		a = analyze(repoDir, targetAbs, sha, explicitSource, opts.Verbose)
		if a.Err != nil {
			return fallbackToStock(opts, fmt.Errorf("analyze: %w", a.Err))
		}
		if !a.SrcOK {
			return fallbackToStock(opts, errors.New("no viable source worktree"))
		}
		if err := cowPreflight(a.Src.Path, targetAbs); err != nil {
			return fallbackToStock(opts, err)
		}
		phase("preflight", t)
	}
	writeBackendLabel("cow")

	refsBefore, err := localBranchRefs(repoDir)
	if err != nil {
		return fmt.Errorf("snapshot branches: %w", err)
	}
	targetParent := filepath.Dir(targetAbs)
	if err := os.MkdirAll(targetParent, 0o777); err != nil {
		return fmt.Errorf("create target parent: %w", err)
	}
	stagingParent, err := os.MkdirTemp(targetParent, ".git-cow-worktree-staging-")
	if err != nil {
		return fmt.Errorf("create private staging directory: %w", err)
	}
	if err := os.Chmod(stagingParent, 0o700); err != nil {
		os.RemoveAll(stagingParent)
		return fmt.Errorf("secure private staging directory: %w", err)
	}
	defer os.RemoveAll(stagingParent)
	// Git's --guess-remote keys off the target basename, so preserve it in
	// staging rather than using a generic temporary leaf name.
	stagingAbs := filepath.Join(stagingParent, filepath.Base(targetAbs))
	txn := worktreeTransaction{repoDir: repoDir, targetAbs: stagingAbs, refsBefore: refsBefore}
	defer func() {
		if retErr == nil || !txn.registered {
			return
		}
		if rollbackErr := txn.rollback(); rollbackErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rollbackErr))
		}
	}()

	// Register without checking out. In legacy mode, overlap source analysis
	// with Git's metadata work. Auto mode already analyzed synchronously so
	// it could make its preflight/fallback decision before this point.
	addArgs := append([]string{"worktree", "add", "--no-checkout"}, opts.Forward...)
	if opts.UseSeparator {
		addArgs = append(addArgs, "--")
	}
	addArgs = append(addArgs, stagingAbs)
	if opts.CommitIsh != "" {
		addArgs = append(addArgs, opts.CommitIsh)
	}
	t := time.Now()
	if a == nil {
		var addErr error
		var wg sync.WaitGroup
		wg.Go(func() { addErr = runGitStreaming(repoDir, addArgs...) })
		if sha, err := resolveRev(repoDir, guessRev); err == nil {
			a = analyze(repoDir, stagingAbs, sha, explicitSource, opts.Verbose)
		}
		wg.Wait()
		if addErr != nil {
			return fmt.Errorf("git worktree add: %w", addErr)
		}
	} else if err := runGitStreaming(repoDir, addArgs...); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	txn.registered = true
	phase("worktree add", t)

	// Resolve the actual target and remember the ref state this transaction
	// produced, so rollback can use compare-and-swap rather than overwriting
	// somebody else's concurrent branch update.
	targetSHA, err := resolveRev(stagingAbs, "HEAD")
	if err != nil {
		return err
	}
	txn.captureCreatedRef(targetSHA)
	if os.Getenv("GIT_COW_WORKTREE_FAIL_AFTER_ADD") != "" {
		return errors.New("injected failure after worktree registration")
	}
	if a == nil || a.SHA != targetSHA {
		t = time.Now()
		a = analyze(repoDir, stagingAbs, targetSHA, explicitSource, opts.Verbose)
		phase("analyze", t)
	}

	var seededIndex string
	if a.SrcOK {
		if opts.Verbose {
			fmt.Fprintf(os.Stderr, "%s: source=%s (diff=%d commits)\n", commandName, a.Src.Path, a.Score)
		}
		seededIndex, err = seed(a, stagingAbs, phase, opts.Verbose)
		if err != nil && opts.Verbose {
			fmt.Fprintf(os.Stderr, "%s: seeding skipped: %v\n", commandName, err)
		}
	} else if opts.Verbose {
		fmt.Fprintf(os.Stderr, "%s: no viable source worktree; using plain checkout\n", commandName)
	}

	// Checkout and hook are separate phases. A seeded-index failure may be
	// retried, but a hook failure is returned exactly once and never causes a
	// second checkout/hook invocation.
	t = time.Now()
	if err := checkout(stagingAbs); err != nil {
		if seededIndex == "" {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s: checkout failed against seeded index (%v); retrying from scratch\n", commandName, err)
		// The index is ours. index.lock is not: it may have been created by
		// Git or another process, so never unlink it here.
		if rmErr := os.Remove(seededIndex); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return errors.Join(err, fmt.Errorf("remove seeded index: %w", rmErr))
		}
		if err := checkout(stagingAbs); err != nil {
			return err
		}
	}
	phase("checkout", t)

	t = time.Now()
	if err := validateWorktree(stagingAbs, targetSHA); err != nil {
		return fmt.Errorf("validate staged worktree: %w", err)
	}
	phase("correctness validation", t)

	if err := moveWorktree(repoDir, stagingAbs, targetAbs); err != nil {
		return fmt.Errorf("publish worktree: %w", err)
	}
	txn.targetAbs = targetAbs

	// Publication is the transactional boundary. Hooks run in the final path
	// exactly once; after hook execution starts, preserve the worktree even if
	// the hook fails, matching native Git's retained-worktree semantics.
	txn.committed = true
	t = time.Now()
	if err := runPostCheckoutHook(targetAbs, targetSHA); err != nil {
		return err
	}
	phase("post-checkout hook", t)

	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "%s: %-20s %v\n", commandName, "total", time.Since(overallStart).Round(time.Millisecond))
	}
	return nil
}

func fallbackToStock(opts Options, reason error) error {
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "%s: CoW preflight unavailable (%v); using stock git worktree add\n", commandName, reason)
	}
	writeBackendLabel("stock")
	return execGitWorktreeAdd(opts.GitArgs)
}

func writeBackendLabel(backend string) {
	if os.Getenv("SQ_INVOKED_BY_SQ") == "" || backendLabelWritten {
		return
	}
	writeMetricsLabels("backend:" + backend)
	backendLabelWritten = true
}

// analysis is everything the clone plan needs that depends only on the
// repository and the target commit, and not on the new worktree existing.
// Keeping that boundary sharp is what lets the work overlap `git worktree
// add`.
type analysis struct {
	SHA        string // target commit the rest of this describes
	Src        Worktree
	SrcOK      bool
	Score      int // commits between Src and the target
	TargetTree *treeIndex
	SrcTree    *treeIndex
	Excluded   map[string]bool
	Err        error
}

// analyze settles on a source worktree to clone targetSHA from and reads
// what planning needs from both sides. If explicit is non-nil it is used as
// the source, whatever shape it turns out to be in.
//
// Being nearest in commits doesn't make a worktree worth cloning from. One
// that Git is midway through checking out — including one this command is
// creating in another terminal — is at the target commit and yet holds
// almost nothing, so it would contribute nothing but the cost of finding
// that out. Reject those and move down the ranking.
//
// The target tree is the same whichever source wins, so it is read once,
// alongside the first candidate.
func analyze(repoDir, targetAbs, targetSHA string, explicit *Worktree, verbose bool) *analysis {
	a := &analysis{SHA: targetSHA}

	var ranked []scoredSource
	if explicit != nil {
		score, _ := commitDistance(repoDir, explicit.HEAD, targetSHA)
		ranked = []scoredSource{{WT: *explicit, Score: score}}
	} else {
		ranked = rankAutoSources(repoDir, targetAbs, targetSHA, verbose)
	}
	if len(ranked) == 0 {
		return a
	}

	var targetErr error
	var wg sync.WaitGroup
	wg.Go(func() { a.TargetTree, targetErr = lsTree(repoDir, targetSHA) })

	for i, c := range ranked {
		if i == maxSourceAttempts {
			break
		}
		r, err := readSource(c.WT)
		switch {
		case err != nil:
			if verbose {
				fmt.Fprintf(os.Stderr, "%s: skipping source %s: %v\n", commandName, c.WT.Path, err)
			}
			continue
		case explicit == nil && !r.usable():
			if verbose {
				fmt.Fprintf(os.Stderr, "%s: skipping source %s: %d of %d tracked files differ from its HEAD\n",
					commandName, c.WT.Path, r.Dirty, len(r.Tree.Blobs))
			}
			continue
		}
		a.Src, a.Score, a.SrcOK = c.WT, c.Score, true
		a.SrcTree, a.Excluded = r.Tree, r.Excluded
		break
	}

	wg.Wait()
	a.Err = targetErr
	return a
}

// sourceReport is a candidate source worktree seen up close.
type sourceReport struct {
	Tree     *treeIndex
	Excluded map[string]bool
	Dirty    int // tracked paths whose working-tree copy differs from HEAD
}

func readSource(wt Worktree) (*sourceReport, error) {
	var r sourceReport
	var treeErr, exclErr error
	var wg sync.WaitGroup
	wg.Go(func() { r.Tree, treeErr = lsTree(wt.Path, wt.HEAD) })
	wg.Go(func() { r.Excluded, r.Dirty, exclErr = sourceExclusions(wt.Path, wt.HEAD) })
	wg.Wait()
	return &r, cmp.Or(treeErr, exclErr)
}

// usable reports whether the worktree holds enough of its own commit to be
// worth cloning from. A worktree Git hasn't finished checking out reads as
// almost entirely dirty, because every file not yet written differs from
// HEAD; so does one a user has left in a comparable state, and neither is a
// good source. Ordinary local work doesn't come close to the threshold.
func (r *sourceReport) usable() bool {
	return r.Dirty*2 <= len(r.Tree.Blobs)
}

// seed reflinks as much of the new worktree as it can from src, and writes
// an index vouching for what it reflinked so the checkout that follows
// leaves those files — and their shared storage — alone. It returns the
// path of the index it wrote, if any.
//
// Every step is best-effort. Returning an error, or returning nothing,
// means the caller's checkout writes the worktree the ordinary way.
func seed(a *analysis, targetAbs string, phase func(string, time.Time), verbose bool) (string, error) {
	if a.Err != nil {
		return "", a.Err
	}

	// Sparse checkout is worktree-local configuration, so unlike the rest of
	// the analysis this has to wait for the new worktree to exist.
	t := time.Now()
	targetTree := a.TargetTree
	sparse, err := sparseCheckoutEnabled(targetAbs)
	if err != nil {
		return "", err
	}
	if sparse {
		// Only some of a directory's files get materialized, so cloning
		// whole directories would import files the user excluded.
		targetTree, err = filterSparseCheckout(targetAbs, targetTree)
		if err != nil {
			return "", err
		}
	}
	conversion, err := conversionExclusions(a.Src.Path, a.SHA, targetTree)
	if err != nil {
		return "", err
	}
	excluded := make(map[string]bool, len(a.Excluded)+len(conversion))
	for path := range a.Excluded {
		excluded[path] = true
	}
	for path := range conversion {
		excluded[path] = true
	}
	phase("sparse and attributes", t)

	// GIT_COW_WORKTREE_NO_DIR_CLONE exercises the file-by-file path, which
	// is what Linux always takes, on a filesystem that can clone directories.
	cloneDirs := canCloneDirs && !sparse && os.Getenv("GIT_COW_WORKTREE_NO_DIR_CLONE") == ""

	t = time.Now()
	plan := planClones(a.SrcTree, targetTree, excluded, cloneDirs)
	phase("plan", t)

	t = time.Now()
	result := plan.clone(a.Src.Path, targetAbs)
	phase("clone", t)
	if verbose {
		fmt.Fprintf(os.Stderr, "%s: planned %s; cloned %d dirs, %d files\n",
			commandName, plan, result.Dirs, result.Files)
	}
	if result.StopErr != nil {
		return "", fmt.Errorf("stopped cloning: %w", result.StopErr)
	}
	if result.Dirs == 0 && result.Files == 0 {
		return "", nil
	}

	t = time.Now()
	entries := validateClones(targetAbs, targetTree, plan)
	phase("validate", t)
	if verbose {
		fmt.Fprintf(os.Stderr, "%s: verified %d files\n", commandName, len(entries))
	}
	if len(entries) == 0 {
		return "", nil
	}

	t = time.Now()
	path, err := indexPath(targetAbs)
	if err != nil {
		return "", err
	}
	if err := writeIndex(path, entries); err != nil {
		return "", err
	}
	phase("write index", t)
	return path, nil
}

func resolveFromPath(fromPath string) (Worktree, error) {
	abs, err := osAbs(fromPath)
	if err != nil {
		return Worktree{}, fmt.Errorf("--from invalid: %w", err)
	}
	sha, err := resolveRev(abs, "HEAD")
	if err != nil {
		return Worktree{}, fmt.Errorf("--from %s: %w", abs, err)
	}
	return Worktree{Path: abs, HEAD: sha}, nil
}

func rankAutoSources(repoDir, targetAbs, targetSHA string, verbose bool) []scoredSource {
	all, err := listWorktrees(repoDir)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "%s: list worktrees: %v\n", commandName, err)
		}
		return nil
	}
	pool := candidatePool(all, repoDir, targetAbs, maxOtherCandidates)
	if len(pool) == 0 {
		return nil
	}
	return rankSources(repoDir, pool, targetSHA)
}

func parseArgs(args []string) (Options, error) {
	var o Options
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.GitArgs = append(o.GitArgs, a)
			o.UseSeparator = true
			positional = append(positional, args[i+1:]...)
			o.GitArgs = append(o.GitArgs, args[i+1:]...)
			i = len(args)
		case a == "-v" || a == "--verbose":
			o.Verbose = true
			// Not forwarded to git: `git worktree add` doesn't accept -v.
		case a == "--cow":
			if i+1 >= len(args) {
				return o, errors.New("--cow requires auto or off")
			}
			i++
			if err := setCowMode(&o, args[i]); err != nil {
				return o, err
			}
		case strings.HasPrefix(a, "--cow="):
			if err := setCowMode(&o, strings.TrimPrefix(a, "--cow=")); err != nil {
				return o, err
			}
		case a == "--from":
			if i+1 >= len(args) {
				return o, errors.New("--from requires a path")
			}
			o.FromPath = args[i+1]
			o.FromSpecified = true
			i++
		case strings.HasPrefix(a, "--from="):
			o.FromPath = strings.TrimPrefix(a, "--from=")
			if o.FromPath == "" {
				return o, errors.New("--from requires a path")
			}
			o.FromSpecified = true
		case a == "--checkout":
			o.NoCheckout = false
			o.GitArgs = append(o.GitArgs, a)
		case a == "--no-checkout":
			o.NoCheckout = true
			o.GitArgs = append(o.GitArgs, a)
		case a == "--orphan":
			o.Orphan = true
			o.GitArgs = append(o.GitArgs, a)
		case a == "--no-orphan":
			o.Orphan = false
			o.GitArgs = append(o.GitArgs, a)
		case strings.HasPrefix(a, "-"):
			arity := gitWorktreeAddFlagArity(a)
			if arity == 1 {
				if i+1 >= len(args) {
					return o, fmt.Errorf("%s requires an argument", a)
				}
				o.Forward = append(o.Forward, a, args[i+1])
				o.GitArgs = append(o.GitArgs, a, args[i+1])
				i++
			} else {
				o.Forward = append(o.Forward, a)
				o.GitArgs = append(o.GitArgs, a)
			}
		default:
			positional = append(positional, a)
			o.GitArgs = append(o.GitArgs, a)
		}
	}
	if len(positional) > 0 {
		o.TargetPath = positional[0]
	}
	if len(positional) > 1 {
		o.CommitIsh = positional[1]
	}
	if len(positional) > 2 {
		return o, fmt.Errorf("unexpected positional argument: %s", positional[2])
	}
	return o, nil
}

func setCowMode(o *Options, value string) error {
	switch cowMode(value) {
	case cowModeAuto, cowModeOff:
		o.CowMode = cowMode(value)
		return nil
	default:
		return fmt.Errorf("invalid --cow value %q (want auto or off)", value)
	}
}

func gitWorktreeAddFlagArity(arg string) int {
	switch arg {
	case "-b", "-B", "--reason":
		return 1
	}
	return 0
}

func execGitWorktreeAdd(args []string) error {
	cmd := exec.Command("git", append([]string{"worktree", "add"}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runGitStreaming(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
