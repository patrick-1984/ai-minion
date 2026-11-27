package config

// File transfer on the Worker (files.v1, v0.24.0, BRD FR-18, FR-3, FR-8): the
// local switch and the two local relaxations. They are local settings only:
// the Hub cannot change them, and a narrowing value saved in the config file
// holds against a wider value from a flag or the environment.

// Names of the Worker file transfer settings.
const (
	// KeyFileTransfer is the local switch: on (default), send_only,
	// receive_only or off (the ops.FileMode* values). It is read again at
	// every request and before every publish.
	KeyFileTransfer = "file_transfer"
	// KeyFileTransferAllowHome allows the home folder itself as the folder of
	// a transfer (default false).
	KeyFileTransferAllowHome = "file_transfer_allow_home"
	// KeyFileTransferProtected keeps (on, default) or relaxes (off) the
	// push-protected names and folders (.git/hooks, .claude, shell rc files
	// and so on). The floor and the sensitive folders always stay.
	KeyFileTransferProtected = "file_transfer_protected"
)

// Values of file_transfer. They are the ops.FileMode* values; config does not
// import ops.
const (
	FileTransferOn          = "on"
	FileTransferSendOnly    = "send_only"
	FileTransferReceiveOnly = "receive_only"
	FileTransferOff         = "off"
)

func init() { declareWorkerFiles(Default) }

func declareWorkerFiles(r *Registry) {
	r.Declare(Setting{
		Key:   KeyFileTransfer,
		Scope: ScopeWorker,
		Help: "File transfer with the folders of this Worker's sessions: on (default), send_only (files may leave, none " +
			"arrive), receive_only (files may arrive, none leave) or off. Local setting; the Hub cannot lift it. A change " +
			"applies to the next request and stops a publish that has not happened yet. A narrower value in the config " +
			"file holds against a wider one in the environment",
		Default: FileTransferOn,
		Choices: []string{FileTransferOn, FileTransferSendOnly, FileTransferReceiveOnly, FileTransferOff},
	})
	r.Declare(Setting{
		Key:   KeyFileTransferAllowHome,
		Scope: ScopeWorker,
		Help: "Allow file transfer when a session's folder is the home folder itself (true or false, default false). " +
			"Sensitive folders such as ~/.ssh are never transferred",
		Default: "false",
		Choices: []string{"true", "false"},
	})
	r.Declare(Setting{
		Key:   KeyFileTransferProtected,
		Scope: ScopeWorker,
		Help: "Protect project and agent configuration from pushed files (on, default) or not (off): .git/hooks, " +
			".git/config, .env files, shell rc files, .claude, .mcp.json, CI definitions and similar names. off is " +
			"recorded in the transfer history. System, home and sensitive folders stay refused either way",
		Default: "on",
		Choices: []string{"on", "off"},
	})
}

// fileModeRank orders the switch values for the intersection: the bits are
// send (1) and receive (2).
func fileModeBits(m string) (int, bool) {
	switch m {
	case FileTransferOn:
		return 3, true
	case FileTransferSendOnly:
		return 1, true
	case FileTransferReceiveOnly:
		return 2, true
	case FileTransferOff:
		return 0, true
	}
	return 0, false
}

func fileModeOf(bits int) string {
	switch bits {
	case 3:
		return FileTransferOn
	case 1:
		return FileTransferSendOnly
	case 2:
		return FileTransferReceiveOnly
	}
	return FileTransferOff
}

// NarrowFileTransfer returns what two switch values allow together: the
// directions both allow. An unknown value allows nothing.
func NarrowFileTransfer(a, b string) string {
	x, okA := fileModeBits(a)
	y, okB := fileModeBits(b)
	if !okA || !okB {
		return FileTransferOff
	}
	return fileModeOf(x & y)
}

// ResolveFileTransfer returns the local switch in force. It reads the config
// file again at every call, so that `minion worker config set file_transfer`
// applies to the next request without a restart, and it fails closed: a
// config file that cannot be read or a value that cannot be resolved is off.
// The value saved in the file narrows the resolved one (a flag or the
// environment can narrow the file, never widen it).
func ResolveFileTransfer(res *Resolver, configPath string) string {
	if res == nil {
		return FileTransferOn
	}
	r := *res
	file, err := LoadFile(configPath)
	if err != nil {
		return FileTransferOff
	}
	r.File = file
	v, err := r.Resolve(KeyFileTransfer)
	if err != nil {
		return FileTransferOff
	}
	mode := NarrowFileTransfer(v.Value, FileTransferOn)
	if saved, ok, err := file.Lookup(KeyFileTransfer); err != nil {
		return FileTransferOff
	} else if ok {
		mode = NarrowFileTransfer(mode, saved)
	}
	return mode
}

// FileTransferLocal are the two local relaxations of the floor and the lists.
type FileTransferLocal struct {
	// AllowHome allows the home folder itself as a root.
	AllowHome bool
	// ProtectedOff relaxes the push-protected names and folders.
	ProtectedOff bool
}

// ResolveFileTransferLocal returns the relaxations in force. Each needs the
// relaxing value from the resolver and no stricter value saved in the file;
// anything that cannot be read keeps the strict default.
func ResolveFileTransferLocal(res *Resolver, configPath string) FileTransferLocal {
	if res == nil {
		return FileTransferLocal{}
	}
	r := *res
	file, err := LoadFile(configPath)
	if err != nil {
		return FileTransferLocal{}
	}
	r.File = file
	relaxed := func(key, want string) bool {
		v, err := r.Resolve(key)
		if err != nil || v.Value != want {
			return false
		}
		saved, ok, err := file.Lookup(key)
		return err == nil && (!ok || saved == want)
	}
	return FileTransferLocal{
		AllowHome:    relaxed(KeyFileTransferAllowHome, "true"),
		ProtectedOff: relaxed(KeyFileTransferProtected, "off"),
	}
}
