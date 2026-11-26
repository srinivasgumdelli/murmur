package main

import "testing"

func TestListenPrivateEnrollmentRefusesWildcard(t *testing.T) {
	for _, addr := range []string{":4445", "0.0.0.0:4445", "[::]:4445"} {
		if l, err := listenPrivateEnrollment(addr); err == nil {
			l.Close()
			t.Fatalf("listenPrivateEnrollment(%q) succeeded, want refusal", addr)
		}
	}
	l, err := listenPrivateEnrollment("127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	l.Close()
}
