// Command icon renders Pith's app icon to resources/icon.png, which
// mygo build makes the app's icon from.
//
//	go run ./cmd/icon
package main

import (
	"image/png"
	"log"
	"os"

	pithui "github.com/Kel077/pith/internal/ui"
)

func main() {
	f, err := os.Create("resources/icon.png")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, pithui.AppIcon(1024)); err != nil {
		log.Fatal(err)
	}
}
