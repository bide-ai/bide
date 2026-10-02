// Command unlisted-demo makes a gsm machine and is not in .github/gsm-gate/machines.txt, to show
// the gsm machine gate's coverage scan failing. Removed once the failure is recorded.
package main

import (
	"fmt"

	"github.com/blackwell-systems/gsm"
)

func main() {
	r := gsm.NewRegistry("unlisted")
	r.Bool("flag")
	_, _, err := r.Build()
	fmt.Println("Build error:", err)
}
