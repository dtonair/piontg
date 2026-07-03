package authz

import "testing"

func TestAuthorizerAllowsOnlyConfiguredUser(t *testing.T) {
	a := New(42)
	if !a.IsAllowed(42) {
		t.Fatal("expected configured user to be allowed")
	}
	if a.IsAllowed(7) {
		t.Fatal("expected other user to be rejected")
	}
	if New(0).IsAllowed(42) {
		t.Fatal("zero allowed user must not allow anyone")
	}
}
