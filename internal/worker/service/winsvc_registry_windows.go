//go:build windows

package service

import (
	"errors"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const serviceKeyPath = `SYSTEM\CurrentControlSet\Services\` + WindowsServiceName

// serviceKeyGone reports whether the registry key of the service is gone.
// The SCM forgets a deleted service before its key is removed; until then
// writing into the key fails ("marked for deletion") or is lost with it.
func serviceKeyGone() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, registry.ErrNotExist)
	}
	_ = k.Close()
	return false
}

// setServiceEnvironment writes env as the Environment value of the service
// key, sorted, unless it holds exactly that already.
func setServiceEnvironment(env map[string]string) error {
	if len(env) == 0 {
		return nil
	}
	want := make([]string, 0, len(env))
	for k, v := range env {
		want = append(want, k+"="+v)
	}
	sort.Strings(want)
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	if got, _, err := k.GetStringsValue("Environment"); err == nil {
		sort.Strings(got)
		if strings.Join(got, "\x00") == strings.Join(want, "\x00") {
			return nil
		}
	}
	return k.SetStringsValue("Environment", want)
}

// removeServiceKey removes the registry key of the service and its subkeys:
// an orphan that a failed creation left behind.
func removeServiceKey() error {
	return deleteKeyTree(registry.LOCAL_MACHINE, serviceKeyPath)
}

func deleteKeyTree(root registry.Key, path string) error {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	subs, err := k.ReadSubKeyNames(-1)
	_ = k.Close()
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if err := deleteKeyTree(root, path+`\`+sub); err != nil {
			return err
		}
	}
	err = registry.DeleteKey(root, path)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}
