package ui

import (
	"fmt"
	"strings"
)

// Pith speaks English and Simplified Chinese. Strings are written where
// they show, in both languages: tr("Rules", "规则").

// lang is the language the interface speaks: "en" or "zh-Hans". The view
// reads it on the main thread; setLanguage changes it there.
var lang = "zh-Hans"

func zh() bool { return lang == "zh-Hans" }

// Tr returns the string in the interface's language, for the parts of the
// app outside this package: menus of the menu bar icon.
func Tr(en, zhs string) string { return tr(en, zhs) }

// tr returns the string in the interface's language.
func tr(en, zhs string) string {
	if zh() {
		return zhs
	}
	return en
}

// trf formats the string in the interface's language.
func trf(en, zhs string, args ...any) string { return fmt.Sprintf(tr(en, zhs), args...) }

// resolveLanguage picks the language for a setting ("" for the system's)
// and the system's preferred languages.
func resolveLanguage(setting string, preferred []string) string {
	if setting == "en" || setting == "zh-Hans" {
		return setting
	}
	for _, p := range preferred {
		p = strings.ToLower(p)
		switch {
		case strings.HasPrefix(p, "zh"):
			return "zh-Hans"
		case strings.HasPrefix(p, "en"):
			return "en"
		}
	}
	return "en"
}

// list joins names as the language lists them: "A, B and C", "A、B 和 C".
func list(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	head := names[:len(names)-1]
	if zh() {
		return strings.Join(head, "、") + " 和 " + names[len(names)-1]
	}
	return strings.Join(head, ", ") + " and " + names[len(names)-1]
}
