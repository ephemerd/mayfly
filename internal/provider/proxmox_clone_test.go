package provider

import "testing"

// With no VMStorage the clone must stay a LINKED clone on the template's
// storage — Full=false and no Storage override — so an existing pool provisions
// byte-for-byte the way it did before vm_storage existed. This backward-compat
// property is the whole point of leaving the field empty.
func TestCloneOptions_LinkedByDefault(t *testing.T) {
	opts := cloneOptions(CreateSpec{ID: 123, Name: "mfl-x-123", Node: "node-b"})

	if opts.NewID != 123 || opts.Name != "mfl-x-123" || opts.Target != "node-b" {
		t.Errorf("clone identity = %+v, want NewID/Name/Target passed through", opts)
	}
	if bool(opts.Full) {
		t.Error("Full = true, want a LINKED clone (Full=false) when VMStorage is empty")
	}
	if opts.Storage != "" {
		t.Errorf("Storage = %q, want empty so the disk stays on the template's storage", opts.Storage)
	}
}

// With VMStorage set the clone must become a FULL clone whose copied disk lands
// on that storage — Full=true and Storage=<vm_storage>. That is what puts the
// VM disk on a bigger/separate store than the import-capable template storage.
func TestCloneOptions_FullWhenVMStorageSet(t *testing.T) {
	opts := cloneOptions(CreateSpec{ID: 200, Name: "mfl-node-b-200", Node: "node-b", VMStorage: "local-lvm"})

	if !bool(opts.Full) {
		t.Error("Full = false, want a FULL clone (Full=true) when VMStorage is set")
	}
	if opts.Storage != "local-lvm" {
		t.Errorf("Storage = %q, want the copied disk targeted at local-lvm", opts.Storage)
	}
	if opts.NewID != 200 || opts.Name != "mfl-node-b-200" || opts.Target != "node-b" {
		t.Errorf("clone identity = %+v, want NewID/Name/Target still passed through", opts)
	}
}
