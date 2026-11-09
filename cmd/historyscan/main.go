// Command historyscan runs the repository history secret scan. See
// internal/tools/historyscan.
//
// Usage:
//
//	historyscan <repository> <allowlist file>
//	historyscan -lines <repository>
//
// The first checks the history against the allowlist. The second prints an
// allowlist line for every hit, to be read by a person before it is added:
// a line allows exactly one version of one file.
package main

import (
	"fmt"
	"os"

	"github.com/smrit-ai/cli/internal/tools/historyscan"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "-lines" {
		res, err := historyscan.Scan(os.Args[2], nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		for _, l := range historyscan.Lines(res) {
			fmt.Println(l)
		}
		return
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: historyscan <repository> <allowlist file>\n       historyscan -lines <repository>")
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
		fmt.Printf("%s commit %s path %s pattern %s blob %s\n", tag, h.Commit, h.Path, h.Pattern, h.Blob)
	}
	if n := len(res.Findings()); n > 0 {
		fmt.Fprintf(os.Stderr, "%d unexplained hits\n", n)
		os.Exit(1)
	}
	fmt.Println("no unexplained hits")
}
