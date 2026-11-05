package ui

import (
	"strings"

	"github.com/Kel077/pith/locales"
)

// The interface's messages live in the catalogs of package locales:
// locales/en.json, which describes each, and a catalog per language. MyGo
// generates a function per message, which says it in the app's language.

// list joins names as the app's language lists them: "A, B and C".
func list(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	head := strings.Join(names[:len(names)-1], locales.ListComma())
	return locales.ListAnd(head, names[len(names)-1])
}
