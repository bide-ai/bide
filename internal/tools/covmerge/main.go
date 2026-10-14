// Command covmerge merges Go coverage profiles into one, taking the highest count for each block.
//
// With -coverpkg, every package's test run reports the same blocks of the packages it covers, so
// concatenating profiles repeats blocks and Codecov would count each repeat as another statement.
// covmerge keeps one line per block (same file, range and statement count) with the largest
// count seen, so a block is covered when any test run covered it.
//
// Usage:
//
//	go run ./internal/tools/covmerge -o coverage.out a.out b.out ...
//
// All profiles must use the same mode (set, count or atomic). Without -o the result goes to
// standard output. The exit status is 0 on success and 2 on bad input.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// merge reads the profiles and writes the merged profile to w.
func merge(w io.Writer, profiles []io.Reader) error {
	mode := ""
	counts := map[string]int64{} // "file:range numstmt" -> highest count
	for i, r := range profiles {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		first := true
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if line == "" {
				continue
			}
			if first {
				first = false
				m, ok := strings.CutPrefix(line, "mode: ")
				if !ok {
					return fmt.Errorf("profile %d: first line %q is not a mode line", i+1, line)
				}
				if mode != "" && m != mode {
					return fmt.Errorf("profile %d: mode %q differs from %q", i+1, m, mode)
				}
				mode = m
				continue
			}
			sp := strings.LastIndexByte(line, ' ')
			if sp < 0 {
				return fmt.Errorf("profile %d line %d: malformed block %q", i+1, n, line)
			}
			c, err := strconv.ParseInt(line[sp+1:], 10, 64)
			if err != nil {
				return fmt.Errorf("profile %d line %d: bad count in %q", i+1, n, line)
			}
			key := line[:sp]
			if old, ok := counts[key]; !ok || c > old {
				counts[key] = c
			}
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("profile %d: %w", i+1, err)
		}
	}
	if mode == "" {
		return fmt.Errorf("no profiles with a mode line")
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "mode: %s\n", mode)
	for _, k := range keys {
		fmt.Fprintf(bw, "%s %d\n", k, counts[k])
	}
	return bw.Flush()
}

func main() {
	out := flag.String("o", "", "output file (default standard output)")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "covmerge: no profiles given")
		os.Exit(2)
	}
	var rs []io.Reader
	for _, p := range flag.Args() {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "covmerge:", err)
			os.Exit(2)
		}
		defer f.Close()
		rs = append(rs, f)
	}
	w := io.Writer(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "covmerge:", err)
			os.Exit(2)
		}
		defer f.Close()
		w = f
	}
	if err := merge(w, rs); err != nil {
		fmt.Fprintln(os.Stderr, "covmerge:", err)
		os.Exit(2)
	}
}
