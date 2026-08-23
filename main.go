// fabrica-init scaffolds a Fabrica project, injects hand-written Spec structs
// (preserving +fabrica: annotations and comments), then generates code.
//
// Steps mirror fab-examples/fab-examples-init.md:
//  1. fabrica init <name> --module .. --group .. --storage-version v1 --storage-type ent --db ..
//  2. (optional) append `replace github.com/openchami/fabrica => <path>` to go.mod
//  3. fabrica add resource <Name>   (once per --resource)
//  4. merge each Spec struct from its parts file into apis/<group>/v1/<name>_types.go
//  5. fabrica generate
//  6. go mod tidy
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"
)

// resourceSpec describes one --resource mapping.
type resourceSpec struct {
	name       string // Fabrica resource name, e.g. Resource, Resource2
	partsFile  string // resolved path to the file holding the hand-written struct
	structName string // struct to merge, defaults to <name>Spec
	typesFile  string // generated file (relative to project dir) to merge into
}

// config holds all resolved CLI options.
type config struct {
	module       string
	group        string
	db           string
	name         string
	dir          string
	partsDir     string
	localFabrica string
	fabrica      string
	force        bool
	tidy         bool
	resources    []resourceSpec
}

func main() {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run executes the full scaffold pipeline.
func run(cfg *config) error {
	if err := validateResources(cfg); err != nil {
		return err
	}
	if err := checkProjectDir(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.dir, 0o755); err != nil {
		return err
	}
	if err := fabricaInit(cfg); err != nil {
		return err
	}
	if cfg.localFabrica != "" {
		if err := appendReplace(cfg); err != nil {
			return err
		}
	}
	for _, r := range cfg.resources {
		if err := addResource(cfg, r); err != nil {
			return err
		}
	}
	for _, r := range cfg.resources {
		if err := mergeSpec(projectDir(cfg), r); err != nil {
			return err
		}
	}
	if err := goimportsTypesFiles(cfg); err != nil {
		return err
	}
	if err := generate(cfg); err != nil {
		return err
	}
	if cfg.tidy {
		return tidy(cfg)
	}
	return nil
}

// --- flag parsing -----------------------------------------------------------

// stringSlice collects repeatable flags.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func parseFlags() (*config, error) {
	var (
		module       = flag.String("module", "", "Go module path, e.g. github.com/openchami/fab-examples (required)")
		group        = flag.String("group", "", "API group, e.g. example.fabrica.dev (required)")
		db           = flag.String("db", "sqlite", "Ent database driver: sqlite, postgres, or mysql")
		name         = flag.String("name", "", "project name (default: basename of --module)")
		dir          = flag.String("dir", ".", "working directory where the project is created")
		partsDir     = flag.String("parts-dir", ".", "base directory for --resource parts files")
		resourcesDir = flag.String("resources", "", "directory of parts files; the *Spec struct in each .go file names the resource")
		localFabrica = flag.String("local-fabrica", "", "if set, append a replace directive for github.com/openchami/fabrica pointing here")
		fabrica      = flag.String("fabrica", "fabrica", "path to the fabrica binary")
		force        = flag.Bool("force", false, "proceed even if the project directory already exists")
		tidy         = flag.Bool("tidy", true, "run 'go mod tidy' at the end")
		res          stringSlice
	)
	flag.Var(&res, "resource", "resource mapping name:partsfile[:StructName] (repeatable)")
	flag.Var(&res, "r", "shorthand for --resource")
	flag.Parse()

	if flag.NFlag() == 0 && flag.NArg() == 0 {
		flag.Usage()
		os.Exit(0)
	}

	if *module == "" {
		return nil, errors.New("--module is required")
	}
	if *group == "" {
		return nil, errors.New("--group is required")
	}
	if len(res) == 0 && *resourcesDir == "" {
		return nil, errors.New("provide --resources <dir> or at least one -r/--resource")
	}

	projName := *name
	if projName == "" {
		projName = path.Base(*module)
	}

	cfg := &config{
		module:       *module,
		group:        *group,
		db:           *db,
		name:         projName,
		dir:          *dir,
		partsDir:     *partsDir,
		localFabrica: *localFabrica,
		fabrica:      *fabrica,
		force:        *force,
		tidy:         *tidy,
	}
	for _, spec := range res {
		r, err := parseResource(spec, *partsDir, *group)
		if err != nil {
			return nil, err
		}
		cfg.resources = append(cfg.resources, r)
	}
	if *resourcesDir != "" {
		dirResources, err := resourcesFromDir(*resourcesDir, *group)
		if err != nil {
			return nil, err
		}
		cfg.resources = mergeResources(cfg.resources, dirResources)
	}
	return cfg, nil
}

// resourcesFromDir builds a resourceSpec for every .go file in dir, deriving
// the resource name from the file's <Name>Spec struct.
func resourcesFromDir(dir, group string) ([]resourceSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var specs []resourceSpec
	for _, e := range entries {
		fn := e.Name()
		if e.IsDir() || !strings.HasSuffix(fn, ".go") || strings.HasSuffix(fn, "_test.go") {
			continue
		}
		path := filepath.Join(dir, fn)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		specName, err := findSpecStruct(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		specs = append(specs, makeResourceSpec(strings.TrimSuffix(specName, "Spec"), path, specName, group))
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no .go parts files found in %s", dir)
	}
	return specs, nil
}

// mergeResources appends adds to base, skipping names already in base.
func mergeResources(base, adds []resourceSpec) []resourceSpec {
	seen := map[string]bool{}
	for _, r := range base {
		seen[r.name] = true
	}
	for _, r := range adds {
		if !seen[r.name] {
			base = append(base, r)
			seen[r.name] = true
		}
	}
	return base
}

// parseResource turns "name:partsfile[:StructName]" into a resourceSpec.
func parseResource(spec, partsDir, group string) (resourceSpec, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return resourceSpec{}, fmt.Errorf("invalid --resource %q: want name:partsfile[:StructName]", spec)
	}
	if parts[0] == "" {
		return resourceSpec{}, fmt.Errorf("invalid --resource %q: empty name", spec)
	}
	partsFile := parts[1]
	if !filepath.IsAbs(partsFile) {
		partsFile = filepath.Join(partsDir, partsFile)
	}
	structName := ""
	if len(parts) == 3 {
		structName = parts[2]
	}
	return makeResourceSpec(parts[0], partsFile, structName, group), nil
}

// makeResourceSpec builds a resourceSpec, defaulting the struct to <Name>Spec
// and the target file to apis/<group>/v1/<name>_types.go.
func makeResourceSpec(rawName, partsFile, structName, group string) resourceSpec {
	name := titleFirst(rawName)
	if structName == "" {
		structName = name + "Spec"
	}
	typesFile := filepath.Join("apis", group, "v1", strings.ToLower(name)+"_types.go")
	return resourceSpec{name: name, partsFile: partsFile, structName: structName, typesFile: typesFile}
}

// --- validation -------------------------------------------------------------

// validateResources checks parts files exist and contain the target struct.
func validateResources(cfg *config) error {
	for _, r := range cfg.resources {
		data, err := os.ReadFile(r.partsFile)
		if err != nil {
			return fmt.Errorf("resource %s: %w", r.name, err)
		}
		if _, err := extractStructBlock(string(data), r.structName); err != nil {
			return fmt.Errorf("parts file %s: expected struct %q for resource %s: %w", r.partsFile, r.structName, r.name, err)
		}
	}
	return nil
}

// checkProjectDir fails if the target project directory already exists.
func checkProjectDir(cfg *config) error {
	p := projectDir(cfg)
	if _, err := os.Stat(p); err == nil && !cfg.force {
		return fmt.Errorf("project directory %s already exists (use --force to proceed)", p)
	}
	return nil
}

// --- pipeline steps ---------------------------------------------------------

func projectDir(cfg *config) string { return filepath.Join(cfg.dir, cfg.name) }

func fabricaInit(cfg *config) error {
	return runCmd(cfg.dir, cfg.fabrica, "init", cfg.name,
		"--module", cfg.module,
		"--group", cfg.group,
		"--storage-version", "v1",
		"--storage-type", "ent",
		"--db", cfg.db)
}

// appendReplace adds a replace directive for the local fabrica checkout.
func appendReplace(cfg *config) error {
	goMod := filepath.Join(projectDir(cfg), "go.mod")
	f, err := os.OpenFile(goMod, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := fmt.Sprintf("\nreplace github.com/openchami/fabrica => %s\n", cfg.localFabrica)
	fmt.Printf("+ append to go.mod: %s", line)
	_, err = f.WriteString(line)
	return err
}

func addResource(cfg *config, r resourceSpec) error {
	return runCmd(projectDir(cfg), cfg.fabrica, "add", "resource", r.name)
}

func generate(cfg *config) error {
	return runCmd(projectDir(cfg), cfg.fabrica, "generate")
}

func tidy(cfg *config) error {
	return runCmd(projectDir(cfg), "go", "mod", "tidy")
}

// mergeSpec replaces the scaffolded Spec struct with the hand-written one.
func mergeSpec(projectDir string, r resourceSpec) error {
	partsData, err := os.ReadFile(r.partsFile)
	if err != nil {
		return err
	}
	parts := string(partsData)
	block, err := extractStructBlock(parts, r.structName)
	if err != nil {
		return fmt.Errorf("%s: %w", r.partsFile, err)
	}
	target := filepath.Join(projectDir, r.typesFile)
	orig, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	merged, err := replaceStructBlock(string(orig), r.structName, block)
	if err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	merged, added, err := appendExtraStructs(merged, parts, r.structName)
	if err != nil {
		return fmt.Errorf("%s: %w", r.partsFile, err)
	}
	if err := os.WriteFile(target, []byte(merged), 0o644); err != nil {
		return err
	}
	fmt.Printf("+ merged %s into %s", r.structName, r.typesFile)
	if len(added) > 0 {
		fmt.Printf(" (+ structs: %s)", strings.Join(added, ", "))
	}
	fmt.Println()
	return nil
}

// appendExtraStructs copies every struct in parts except specStruct into dst.
// A struct already present in dst is skipped when its name ends in "Spec" and
// rejected otherwise. Returns the updated source and the names appended.
func appendExtraStructs(dst, parts, specStruct string) (string, []string, error) {
	existing := structNameSet(dst)
	var blocks, added []string
	for _, name := range structNames(parts) {
		if name == specStruct {
			continue
		}
		if existing[name] {
			if strings.HasSuffix(name, "Spec") {
				continue
			}
			return "", nil, fmt.Errorf("generated code already defines struct %q", name)
		}
		block, err := extractStructBlock(parts, name)
		if err != nil {
			return "", nil, err
		}
		blocks = append(blocks, block)
		added = append(added, name)
	}
	if len(blocks) == 0 {
		return dst, nil, nil
	}
	return strings.TrimRight(dst, "\n") + "\n\n" + strings.Join(blocks, "\n\n") + "\n", added, nil
}

// --- struct block extraction / replacement ----------------------------------

// extractStructBlock returns the leading comment/directive lines plus the full
// `type <structName> struct { ... }` declaration, verbatim.
func extractStructBlock(src, structName string) (string, error) {
	lines := strings.Split(src, "\n")
	start, end, ok := structBlockRange(lines, structName)
	if !ok {
		return "", fmt.Errorf("struct %q not found", structName)
	}
	return strings.Join(lines[start:end+1], "\n"), nil
}

// replaceStructBlock swaps the existing struct block for replacement.
func replaceStructBlock(src, structName, replacement string) (string, error) {
	lines := strings.Split(src, "\n")
	start, end, ok := structBlockRange(lines, structName)
	if !ok {
		return "", fmt.Errorf("struct %q not found", structName)
	}
	out := make([]string, 0, len(lines))
	out = append(out, lines[:start]...)
	out = append(out, strings.Split(replacement, "\n")...)
	out = append(out, lines[end+1:]...)
	return strings.Join(out, "\n"), nil
}

var structDeclRe = regexp.MustCompile(`(?m)^\s*type\s+(\w+)\s+struct\s*{`)

// structNames returns struct type names declared in src, in source order.
func structNames(src string) []string {
	var names []string
	for _, m := range structDeclRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	return names
}

// structNameSet returns the set of struct type names declared in src.
func structNameSet(src string) map[string]bool {
	set := map[string]bool{}
	for _, n := range structNames(src) {
		set[n] = true
	}
	return set
}

// findSpecStruct returns the single struct name ending in "Spec" in src.
func findSpecStruct(src string) (string, error) {
	var found []string
	for _, n := range structNames(src) {
		if strings.HasSuffix(n, "Spec") && n != "Spec" {
			found = append(found, n)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", errors.New("no *Spec struct found")
	default:
		return "", fmt.Errorf("multiple *Spec structs found (%s); use -r name:file:StructName", strings.Join(found, ", "))
	}
}

// structBlockRange returns the [start,end] line indexes of the struct block,
// including any contiguous leading // comment lines.
func structBlockRange(lines []string, structName string) (start, end int, ok bool) {
	re := regexp.MustCompile(`^\s*type\s+` + regexp.QuoteMeta(structName) + `\s+struct\s*{`)
	typeLine := -1
	for i, l := range lines {
		if re.MatchString(l) {
			typeLine = i
			break
		}
	}
	if typeLine == -1 {
		return 0, 0, false
	}
	start = typeLine
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "//") {
		start--
	}
	endLine, found := structEndLine(lines, typeLine)
	if !found {
		return 0, 0, false
	}
	return start, endLine, true
}

// structEndLine finds the line closing the struct opened at startLine.
func structEndLine(lines []string, startLine int) (int, bool) {
	depth := 0
	opened := false
	for i := startLine; i < len(lines); i++ {
		depth += braceDelta(lines[i])
		if strings.Contains(lines[i], "{") {
			opened = true
		}
		if opened && depth == 0 {
			return i, true
		}
	}
	return 0, false
}

// braceDelta counts { minus } on a line, ignoring braces inside strings,
// raw strings (backticks/struct tags), and // comments.
func braceDelta(line string) int {
	delta := 0
	inRaw := false
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inRaw:
			if c == '`' {
				inRaw = false
			}
		case inStr:
			if c == '"' && line[i-1] != '\\' {
				inStr = false
			}
		case c == '`':
			inRaw = true
		case c == '"':
			inStr = true
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return delta
		case c == '{':
			delta++
		case c == '}':
			delta--
		}
	}
	return delta
}

// --- helpers ----------------------------------------------------------------

// runCmd runs name+args in dir, streaming output, and echoes the command.
func runCmd(dir, name string, args ...string) error {
	fmt.Printf("+ %s %s  (in %s)\n", name, strings.Join(args, " "), dir)
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// goimportsTypesFiles runs goimports -w on every merged types file so the
// injected structs get their imports resolved. Runs after all parts are merged.
func goimportsTypesFiles(cfg *config) error {
	bin, err := ensureGoimports()
	if err != nil {
		return err
	}
	for _, r := range cfg.resources {
		if err := runCmd(projectDir(cfg), bin, "-w", r.typesFile); err != nil {
			return fmt.Errorf("goimports %s: %w", r.typesFile, err)
		}
	}
	return nil
}

// ensureGoimports returns a path to goimports, installing it if necessary.
func ensureGoimports() (string, error) {
	if bin, err := exec.LookPath("goimports"); err == nil {
		return bin, nil
	}
	if bin := gopathGoimports(); bin != "" {
		return bin, nil
	}
	if err := runCmd(".", "go", "install", "golang.org/x/tools/cmd/goimports@latest"); err != nil {
		return "", fmt.Errorf("installing goimports: %w", err)
	}
	if bin, err := exec.LookPath("goimports"); err == nil {
		return bin, nil
	}
	if bin := gopathGoimports(); bin != "" {
		return bin, nil
	}
	return "", errors.New("goimports not found on PATH or in GOPATH/bin after install")
}

// gopathGoimports returns the goimports path under GOPATH/bin if it exists.
func gopathGoimports() string {
	out, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		return ""
	}
	gopath := strings.TrimSpace(string(out))
	if gopath == "" {
		return ""
	}
	bin := filepath.Join(gopath, "bin", "goimports")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	return ""
}

// titleFirst upper-cases the first rune (resource -> Resource, resource2 -> Resource2).
func titleFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
