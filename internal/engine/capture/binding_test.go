package capture

import (
	"errors"
	"net"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

type indexedBindingDriver struct {
	fakeDriver
	byName                map[string]int
	byIndex               map[int]net.Interface
	nameReads, indexReads int
}

func (d *indexedBindingDriver) Index(name string) (int, error) {
	d.nameReads++
	index := d.byName[name]
	if index <= 0 {
		return 0, errors.New("interface absent")
	}
	return index, nil
}

func (d *indexedBindingDriver) CheckBinding(name string, index int) error {
	d.indexReads++
	return checkBoundInterface(name, index, func(index int) (*net.Interface, error) {
		iface, ok := d.byIndex[index]
		if !ok {
			return nil, errors.New("index absent")
		}
		return &iface, nil
	})
}

func TestIndexedBindingChecksRejectReusedIdentities(t *testing.T) {
	for _, kind := range []string{"unchanged", "same-name-new-index", "same-index-new-name", "missing", "wrong-returned-index", "epoch", "generation", "fault"} {
		t.Run(kind, func(t *testing.T) {
			d := &indexedBindingDriver{byName: map[string]int{"example0": 1}, byIndex: map[int]net.Interface{1: {Name: "example0", Index: 1}}}
			c := &Capture{Name: "utun42", opts: Options{Driver: d}}
			g, err := NewGuard(c, []api.Interface{{Name: "example0", Up: true, HardwareAddr: "02:00:00:00:00:01"}})
			if err != nil {
				t.Fatal(err)
			}
			binding, err := g.Binding("example0")
			if err != nil || d.nameReads != 1 {
				t.Fatal("new binding did not resolve its current name", binding, err)
			}
			binding.generation = g.generation
			switch kind {
			case "same-name-new-index":
				d.byName["example0"] = 2
				delete(d.byIndex, 1)
				d.byIndex[2] = net.Interface{Name: "example0", Index: 2}
			case "same-index-new-name":
				d.byIndex[1] = net.Interface{Name: "example1", Index: 1}
			case "missing":
				delete(d.byIndex, 1)
			case "wrong-returned-index":
				d.byIndex[1] = net.Interface{Name: "example0", Index: 2}
			case "epoch":
				if err := g.Interfaces([]api.Interface{{Name: "example0", Up: true, HardwareAddr: "02:00:00:00:00:02"}}); err != nil {
					t.Fatal(err)
				}
			case "generation":
				g.generation++
			case "fault":
				g.Revoke(errors.New("synthetic ownership fault"))
			}
			err = binding.Check()
			if kind == "unchanged" && err != nil || kind == "fault" && api.Code(err) != api.ErrNetworkLost || kind != "unchanged" && kind != "fault" && !errors.Is(err, ErrEgressChanged) {
				t.Fatal("binding safety changed", kind, err)
			}
			wantIndexReads := 1
			if kind == "fault" {
				wantIndexReads = 0
			}
			if d.nameReads != 1 || d.indexReads != wantIndexReads {
				t.Fatal("pinned checks still enumerate by name", d.nameReads, d.indexReads)
			}
			if kind == "same-name-new-index" {
				fresh, err := g.Binding("example0")
				if err != nil || fresh.Index != 2 || fresh.Check() != nil || d.nameReads != 2 || binding.Check() == nil {
					t.Fatal("new binding failed to resolve without migrating the old one", fresh, err)
				}
			}
		})
	}
}

func TestBoundInterfaceLookupFailsClosed(t *testing.T) {
	for _, name := range []string{"", "example0"} {
		for _, index := range []int{0, 1} {
			if err := checkBoundInterface(name, index, func(int) (*net.Interface, error) { return nil, nil }); !errors.Is(err, ErrEgressChanged) {
				t.Fatal("missing lookup evidence was accepted")
			}
		}
	}
	if err := checkBoundInterface("example0", 1, func(int) (*net.Interface, error) {
		return &net.Interface{Name: "example0", Index: 1}, errors.New("read failed")
	}); !errors.Is(err, ErrEgressChanged) {
		t.Fatal("partial lookup with an error was accepted", err)
	}
}
