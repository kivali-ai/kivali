//go:build unix

package agentshell

import (
	"context"
	"reflect"
	"syscall"
	"testing"
)

// A daemon that dropped root runs every command as the commands' user
// with no supplementary groups, on the overlay path too; without that
// a command would run as the daemon and could reach its memory and
// environment. A daemon that did not drop (nil user) leaves the
// command's credentials alone.
func TestShellCommandUser(t *testing.T) {
	ctx := context.Background()
	want := &syscall.Credential{Uid: 65532, Gid: 65531, Groups: []uint32{}}
	user := &CommandUser{UID: 65532, GID: 65531}
	for name, overlay := range map[string][2]string{"plain": {}, "overlay": {"/files/subagents/x", "/files/artifacts/private"}} {
		cmd := shellCommand(ctx, "/files", "true", overlay[0], overlay[1], "", user)
		if cmd.SysProcAttr == nil || !reflect.DeepEqual(cmd.SysProcAttr.Credential, want) {
			t.Errorf("%s: SysProcAttr = %+v, want Credential %+v", name, cmd.SysProcAttr, want)
		}
		cmd = shellCommand(ctx, "/files", "true", overlay[0], overlay[1], "", nil)
		if cmd.SysProcAttr != nil && cmd.SysProcAttr.Credential != nil {
			t.Errorf("%s: no user, but Credential %+v", name, cmd.SysProcAttr.Credential)
		}
	}
}
