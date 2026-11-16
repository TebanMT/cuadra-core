package timezone

import (
	_ "embed"
	"strings"
	"time"
	_ "time/tzdata"
)

// Names are the IANA catalogue shipped in the Go toolchain zoneinfo.zip.
// A fixed embedded catalogue excludes host-only files such as localtime or
// posixrules. LoadLocation is used only after membership, with embedded tzdata
// available on systems without an OS zone database. No fallback is permitted.
//
//go:embed iana_names.txt
var catalogue string
var names = func() map[string]bool {
	result := map[string]bool{}
	for _, name := range strings.Fields(catalogue) {
		result[name] = true
	}
	return result
}()

type IANA struct{}

func NewIANA() *IANA { return &IANA{} }
func (*IANA) Valid(name string) bool {
	if name == "" || name == "Local" || !names[name] {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
