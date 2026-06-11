//go:build ignore

// scripts/check-api-docs.go
// Lint: fail if docs/api-reference.md is out of sync with cmd/gateway/main.go.
//
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func projectRoot() string {
	if dir := os.Getenv("PROJECT_ROOT"); dir != "" {
		return dir
	}
	exeDir := filepath.Dir(os.Args[0])
	if strings.Contains(exeDir, "go-build") || strings.Contains(exeDir, "/tmp/") {
		wd, err := os.Getwd()
		if err == nil {
			return wd
		}
	}
	root, err := filepath.Abs(filepath.Join(exeDir, ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: finding project root: %v\n", err)
		os.Exit(1)
	}
	return root
}

// extractRoutes parses cmd/gateway/main.go and returns all registered HTTP routes.
func extractRoutes(mainPath string) ([]string, error) {
	data, err := os.ReadFile(mainPath)
	if err != nil {
		return nil, err
	}

	var routes []string
	re := regexp.MustCompile(`mux\.HandleFunc\("([^"]+)",\s*(?:server\.)?handle`)
	for _, match := range re.FindAllSubmatch(data, -1) {
		routeStr := string(match[1])
		parts := strings.SplitN(routeStr, " ", 2)
		if len(parts) == 2 {
			routes = append(routes, parts[1])
		}
	}

	sort.Strings(routes)
	return routes, nil
}

// extractAuthRulePaths finds the "### Authorization rules" table in the markdown
// and returns every backtick-quoted path string (e.g. "/agent-requests").
func extractAuthRulePaths(docPath string) (map[string]struct{}, error) {
	data, err := os.ReadFile(docPath)
	if err != nil {
		return nil, err
	}
	content := string(data)

	// Find the Authorization rules section.
	sectionStart := strings.Index(content, "### Authorization rules")
	if sectionStart == -1 {
		return nil, fmt.Errorf("'### Authorization rules' section not found in %s", docPath)
	}

	// Look for the next ### heading or end of file.
	nextHeading := strings.Index(content[sectionStart+1:], "\n### ")
	var sectionEnd int
	if nextHeading == -1 {
		sectionEnd = len(content)
	} else {
		sectionEnd = sectionStart + 1 + nextHeading
	}
	section := content[sectionStart:sectionEnd]

	// Extract all backtick-quoted strings that look like paths.
	paths := make(map[string]struct{})
	re := regexp.MustCompile("`([^`]+)`")
	for _, match := range re.FindAllStringSubmatch(section, -1) {
		text := match[1]
		// A path contains a leading slash; skip role names like "admin" or "reviewer".
		if strings.Contains(text, "/") {
			// The auth table may contain cells like:
			//   `GET /agent-requests`, `GET /agent-requests/{name}`
			// Split on commas and clean up.
			for _, part := range strings.Split(text, ",") {
				part = strings.TrimSpace(part)
				// The part may include the HTTP method prefix (e.g. "GET /foo").
				// We only care about the path portion.
				if idx := strings.Index(part, " "); idx != -1 {
					part = part[idx+1:]
				}
				if strings.HasPrefix(part, "/") {
					paths[part] = struct{}{}
				}
			}
		}
	}

	return paths, nil
}

// verifyAuthRules ensures every route registered in main.go appears in the
// Authorization rules table.
func verifyAuthRules(mainPath, docPath string) error {
	routes, err := extractRoutes(mainPath)
	if err != nil {
		return fmt.Errorf("extracting routes: %w", err)
	}

	authPaths, err := extractAuthRulePaths(docPath)
	if err != nil {
		return err
	}

	var missing []string
	for _, route := range routes {
		if _, ok := authPaths[route]; !ok {
			missing = append(missing, route)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		fmt.Fprintf(os.Stderr, "ERROR: docs/api-reference.md authorization rules are missing entries for:\n")
		for _, r := range missing {
			fmt.Fprintf(os.Stderr, "  %s\n", r)
		}
		fmt.Fprintf(os.Stderr, "Add them manually to the '### Authorization rules' table.\n")
		return fmt.Errorf("authorization rules incomplete")
	}

	return nil
}

func main() {
	projectRoot := projectRoot()
	apiDoc := filepath.Join(projectRoot, "docs", "api-reference.md")
	backup := apiDoc + ".bak"

	// ── Check 1: auto-generated endpoint table ────────────────────────────────
	original, err := os.ReadFile(apiDoc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: reading %s: %v\n", apiDoc, err)
		os.Exit(1)
	}

	if err := os.WriteFile(backup, original, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: writing backup: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(backup)

	genPath := filepath.Join(projectRoot, "scripts", "generate-api-docs.go")
	cmd := exec.Command("go", "run", genPath)
	cmd.Dir = projectRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: running generator: %s\n", string(out))
		os.Exit(1)
	}

	generated, err := os.ReadFile(apiDoc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: reading generated %s: %v\n", apiDoc, err)
		os.Exit(1)
	}

	if err := os.WriteFile(apiDoc, original, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: restoring original: %v\n", err)
		os.Exit(1)
	}

	if string(original) != string(generated) {
		fmt.Fprintf(os.Stderr, "ERROR: %s is out of sync with cmd/gateway/main.go\n", apiDoc)
		fmt.Fprintf(os.Stderr, "Run 'make docs-generate' to regenerate.\n")
		diff := exec.Command("diff", "-u", backup, apiDoc+".gen")
		os.WriteFile(apiDoc+".gen", generated, 0644)
		defer os.Remove(apiDoc + ".gen")
		out, _ := diff.CombinedOutput()
		fmt.Fprintf(os.Stderr, "%s\n", string(out))
		os.Exit(1)
	}

	fmt.Println("docs/api-reference.md is up to date.")

	// ── Check 2: authorization rules table ────────────────────────────────────
	mainPath := filepath.Join(projectRoot, "cmd", "gateway", "main.go")
	if err := verifyAuthRules(mainPath, apiDoc); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Authorization rules table is complete.")
}
