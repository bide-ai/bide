package gcs_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/anchor/gcs"
	"github.com/bide-ai/bide/anchor/gcs/gcstest"
	"github.com/bide-ai/bide/audit"
)

func perms(t *testing.T, role string) []string {
	t.Helper()
	p, err := gcs.RolePermissions(role)
	if err != nil || len(p) == 0 {
		t.Fatalf("role: %v, %v", p, err)
	}
	return p
}

func TestRolesGrantNoDeleteOrWeakening(t *testing.T) {
	forbidden := []string{"delete", "setretention", "overrideunlockedretention", "setiampolicy", "buckets.update", "buckets.delete",
		"lockretention", "objects.update", "buckets.create"}
	for name, role := range map[string]string{"writer": gcs.WriterRole, "reader": gcs.ReaderRole} {
		for _, p := range perms(t, role) {
			l := strings.ToLower(p)
			if strings.HasSuffix(l, ".*") {
				t.Errorf("%s role grants the wildcard %s", name, p)
			}
			for _, f := range forbidden {
				if strings.Contains(l, f) {
					t.Errorf("%s role grants %s", name, p)
				}
			}
		}
	}
	if slices.Contains(perms(t, gcs.ReaderRole), "storage.objects.create") {
		t.Error("the reader role can write")
	}
	// The hold releaser can update metadata (release a hold) and nothing that creates or deletes.
	hr := perms(t, gcs.HoldReleaserRole)
	if !slices.Contains(hr, "storage.objects.update") {
		t.Error("the hold releaser cannot release a hold")
	}
	for _, p := range hr {
		if strings.Contains(p, "create") || strings.Contains(p, "delete") || strings.Contains(p, "setRetention") {
			t.Errorf("hold releaser role grants %s", p)
		}
	}
	// The writer cannot release the hold it places (a separate identity must).
	if slices.Contains(perms(t, gcs.WriterRole), "storage.objects.update") {
		t.Error("the writer role can release holds")
	}
}

func TestMethodPermissionsCoverEveryStoreMethod(t *testing.T) {
	typ := reflect.TypeFor[gcs.Store]()
	for i := range typ.NumMethod() {
		m := typ.Method(i).Name
		if len(gcs.MethodPermissions[m]) == 0 {
			t.Errorf("Store.%s has no entry in MethodPermissions", m)
		}
	}
	if len(gcs.MethodPermissions) != typ.NumMethod() {
		t.Errorf("MethodPermissions has %d entries for %d methods", len(gcs.MethodPermissions), typ.NumMethod())
	}
	for m, ps := range gcs.MethodPermissions {
		for _, p := range ps {
			if strings.Contains(p, "delete") {
				t.Errorf("Store.%s needs %s: the package must never call delete", m, p)
			}
		}
	}
}

// allowed fails for every call the fake saw that the role does not allow.
func allowed(t *testing.T, role string, calls []string) {
	t.Helper()
	have := perms(t, role)
	if len(calls) == 0 {
		t.Fatal("no calls were recorded")
	}
	for _, c := range calls {
		for _, p := range gcs.MethodPermissions[c] {
			if !slices.Contains(have, p) {
				t.Errorf("a call to Store.%s needs %s, which the role does not grant", c, p)
			}
		}
	}
}

func TestEveryWriterCallIsAllowedByTheWriterRole(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	a := newAnchor(t, f, func(c *gcs.Config) { c.LegalHold = true })
	h := k.head(t, "r", 1, 1)
	publish(t, a, h)
	publish(t, a, h) // the 412 path: read back of an existing object
	f.LoseResponseNext("Create", gcs.ErrUnavailable)
	_ = a.Publish(ctx, "r", k.head(t, "r", 2, 1))
	allowed(t, gcs.WriterRole, f.Calls())
}

func TestEveryReaderCallIsAllowedByTheReaderRole(t *testing.T) {
	k := newKeys(t, 1)
	f := gcstest.New()
	publish(t, newAnchor(t, f, nil), k.head(t, "r", 1, 1))
	n := len(f.Calls())
	r := newReader(t, f, k.v)
	_, _ = r.Latest(ctx, audit.TreeJournal, "r")
	_, _ = r.Heads(ctx, audit.TreeJournal, "r")
	_, _ = r.Head(ctx, audit.TreeJournal, "r", 1)
	_, _ = r.Audit(ctx, audit.TreeJournal, "r", f.Now().Add(time.Hour))
	allowed(t, gcs.ReaderRole, f.Calls()[n:])
}

func TestObjectRetentionNeedsItsOwnWriterRole(t *testing.T) {
	plain, withRetention := perms(t, gcs.WriterRole), perms(t, gcs.WriterObjectRetentionRole)
	for _, p := range gcs.ObjectRetentionPermissions {
		if slices.Contains(plain, p) {
			t.Errorf("the plain writer role grants %s", p)
		}
		if !slices.Contains(withRetention, p) {
			t.Errorf("the object-retention writer role lacks %s", p)
		}
	}
	// It is the writer role plus exactly those permissions.
	want := slices.Sorted(slices.Values(append(slices.Clone(plain), gcs.ObjectRetentionPermissions...)))
	if got := slices.Sorted(slices.Values(withRetention)); !slices.Equal(got, want) {
		t.Errorf("object-retention writer = %v, want writer plus setRetention = %v", got, want)
	}
	for _, p := range withRetention {
		l := strings.ToLower(p)
		if strings.Contains(l, "delete") || strings.Contains(l, "override") || strings.Contains(l, "setiampolicy") || strings.Contains(l, "buckets.update") || strings.Contains(l, "objects.update") {
			t.Errorf("object-retention writer role grants %s", p)
		}
	}
	k := newKeys(t, 1)
	f := gcstest.New()
	f.Configure(func(b *gcs.BucketInfo) { b.ObjectRetention = true })
	a := newAnchor(t, f, func(c *gcs.Config) { c.ObjectRetention = true })
	publish(t, a, k.head(t, "r", 1, 1))
	allowed(t, gcs.WriterObjectRetentionRole, f.Calls())
	for _, p := range gcs.ObjectRetentionPermissions {
		if !slices.Contains(withRetention, p) {
			t.Errorf("a Create with a retention needs %s", p)
		}
	}
}
