// Command historyscan runs the repository history secret scan. See
// internal/tools/historyscan. Usage: historyscan <repo> <allowlist file>.
package main

import (
	"fmt"
	"os"

	"github.com/smrit-ai/cli/internal/tools/historyscan"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: historyscan <repository> <allowlist file>")
		os.Exit(2)
	}
	text, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	allow, err := historyscan.ParseAllowlist(string(text))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	res, err := historyscan.Scan(os.Args[1], allow)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Printf("scanned %d commits, %d file versions\n", res.Commits, res.Blobs)
	for _, h := range res.Hits {
		tag := "FOUND"
		if h.Allowed {
			tag = "allowed"
		}
		fmt.Printf("%s commit %s path %s pattern %s\n", tag, h.Commit, h.Path, h.Pattern)
	}
	if n := len(res.Findings()); n > 0 {
		fmt.Fprintf(os.Stderr, "%d unexplained hits\n", n)
		os.Exit(1)
	}
	fmt.Println("no unexplained hits")
}
