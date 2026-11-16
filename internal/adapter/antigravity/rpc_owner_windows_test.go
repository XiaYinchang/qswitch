package antigravity

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func rpcTestMetadataACL(t *testing.T, path string, public bool) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Construct fixtures independently of createPrivateMetadataFile so a regression
	// in the writer cannot silently weaken the reader's positive test cases.
	sddl := "O:" + user.User.Sid.String() + "D:P(A;;FA;;;" + user.User.Sid.String() + ")"
	if public {
		sddl += "(A;;GR;;;WD)"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, user.User.Sid, nil, dacl, nil); err != nil {
		t.Fatalf("prepare synthetic Windows metadata ACL: %v", err)
	}
}

func TestWindowsDaemonACLRejectsOtherUserGrants(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	owner := "O:" + user.User.Sid.String()
	private := owner + "D:P(A;;FA;;;" + user.User.Sid.String() + ")"
	for _, tc := range []struct {
		name string
		sddl string
		want bool
	}{
		{"current user", private, true},
		{"system and administrators", private + "(A;;FA;;;SY)(A;;FA;;;BA)", true},
		{"public generic read", private + "(A;;GR;;;WD)", false},
		{"public read data", private + "(A;;0x1;;;WD)", false},
		{"public generic all", private + "(A;;GA;;;WD)", false},
		{"public write data", private + "(A;;0x2;;;WD)", false},
		{"public ACL modification", private + "(A;;WD;;;WD)", false},
		{"inherit only grant", private + "(A;OICIIO;GR;;;WD)", true},
		{"read attributes only", private + "(A;;0x80;;;WD)", true},
		{"different owner", "O:SYD:P(A;;FA;;;SY)", false},
		{"missing DACL", owner, false},
		{"null DACL", owner + "D:NO_ACCESS_CONTROL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if got := privateWindowsDescriptor(sd, user.User.Sid); got != tc.want {
				t.Fatalf("private ACL=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestWindowsMetadataCreatedPrivateBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.json")
	file, err := createPrivateMetadataFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || !privateDaemonOwner(path, info) {
		t.Fatal("metadata file must already be private while still empty")
	}
	if _, err := file.WriteString("synthetic-private-metadata"); err != nil {
		t.Fatal(err)
	}
	if second, err := createPrivateMetadataFile(path); err == nil {
		second.Close()
		t.Fatal("private metadata creation overwrote an existing file")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "synthetic-private-metadata" {
		t.Fatal("exclusive metadata creation changed existing contents")
	}
}
