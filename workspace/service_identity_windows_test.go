//go:build windows

package workspace

import "testing"

func TestDefaultServiceIdentityIsProductNeutralAndDoesNotCollideWithLegacyTestPlay(t *testing.T) {
	const legacyService = "TestPlayStorageBroker"
	const legacyPipe = `\\.\pipe\testplay-storage-broker-v2`

	if WindowsServiceName != "UnityWorkspaceStorage" {
		t.Fatalf("unexpected service name %q", WindowsServiceName)
	}
	if DefaultPipeName != `\\.\pipe\unity-workspace-storage-v3` {
		t.Fatalf("unexpected default pipe %q", DefaultPipeName)
	}
	if WindowsServiceName == legacyService || DefaultPipeName == legacyPipe {
		t.Fatal("neutral storage identity must coexist with the legacy TestPlay broker")
	}
}
