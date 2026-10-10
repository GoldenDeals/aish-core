package aish

import (
	"context"
	"testing"
)

type nopModule struct{}

func (nopModule) Init(context.Context, *Host) error { return nil }

func newNop() Module { return nopModule{} }

func init() {
	RegisterModule(ModuleInfo{ID: "test.registry_b", New: newNop})
	RegisterModule(ModuleInfo{ID: "test.registry_a", New: newNop})
}

func TestRegisterModule(t *testing.T) {
	t.Parallel()

	if _, ok := LookupModule("test.registry_a"); !ok {
		t.Error("registered module not found")
	}
	var ids []string
	for _, info := range Modules() {
		if info.ID == "test.registry_a" || info.ID == "test.registry_b" {
			ids = append(ids, info.ID)
		}
	}
	if len(ids) != 2 || ids[0] != "test.registry_a" {
		t.Errorf("Modules() order = %v, want sorted by ID", ids)
	}

	for _, bad := range []ModuleInfo{
		{ID: "test.registry_a", New: newNop}, // duplicate
		{ID: "noseparator", New: newNop},
		{ID: "Test.Upper", New: newNop},
		{ID: "test.nil_new"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RegisterModule(%+v) did not panic", bad)
				}
			}()
			RegisterModule(bad)
		}()
	}
}
