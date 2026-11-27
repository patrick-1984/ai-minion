package version

import "os"

// aidSystem is Android's system user and group (AID_SYSTEM). The platform
// creates /data, /data/user and /data/data as system:system with mode 0771;
// only the system server and root can write them, and each app's own folder
// below them belongs to the app's uid.
const aidSystem = 1000

// androidTrustedOwner reports whether Android trusts uid as the owner of a
// directory above an app's private storage.
func androidTrustedOwner(uid uint32) bool { return uid == aidSystem }

// androidTrustedGroupWrite accepts group write only on a directory owned by
// system:system that others cannot write (0771 system:system).
func androidTrustedGroupWrite(uid, gid uint32, perm os.FileMode) bool {
	return uid == aidSystem && gid == aidSystem && perm&0o002 == 0
}
