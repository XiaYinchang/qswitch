package antigravity

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func privateDaemonOwner(path string, _ os.FileInfo) bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	return err == nil && privateWindowsDescriptor(sd, user.User.Sid)
}

func privateWindowsDescriptor(sd *windows.SECURITY_DESCRIPTOR, current *windows.SID) bool {
	if sd == nil || !sd.IsValid() || current == nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !windows.EqualSid(owner, current) {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false // A missing or null DACL permits everyone.
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace == nil {
			return false
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false // Unhandled object/conditional grants are not private proof.
		}
		// Reject grants that disclose the CSRF or let another user replace it.
		const sensitive = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_ALL
		if ace.Mask&sensitive == 0 {
			continue
		}
		if uintptr(ace.Header.AceSize) < unsafe.Offsetof(ace.SidStart)+8 {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || (!windows.EqualSid(sid, current) && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)) {
			return false
		}
	}
	return true
}

func privateMetadataSecurity() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("antigravity metadata: obtain current Windows identity: %w", err)
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, fmt.Errorf("antigravity metadata: construct private Windows ACL: %w", err)
	}
	return sd, nil
}

// CREATE_NEW installs the private DACL atomically with file creation. Tightening
// permissions only after opening a public file would leave earlier handles
// readable even after its ACL changed.
func createPrivateMetadataFile(path string) (*os.File, error) {
	sd, err := privateMetadataSecurity()
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, errors.New("antigravity metadata: invalid Windows file path")
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("antigravity metadata: create private Windows file: %w", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}
