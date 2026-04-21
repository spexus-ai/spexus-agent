package cli

import (
	"context"
	"fmt"
	"io"
)

type noopConfigHandler struct {
	out io.Writer
}

func (h noopConfigHandler) Init(context.Context, []string) error {
	return scaffolded(h.out, "config init")
}

func (h noopConfigHandler) Show(context.Context, []string) error {
	return scaffolded(h.out, "config show")
}

func (h noopConfigHandler) Validate(context.Context, []string) error {
	return scaffolded(h.out, "config validate")
}

func (h noopConfigHandler) SetBaseWorkspace(context.Context, []string) error {
	return scaffolded(h.out, "config set-base-workspace")
}

func (h noopConfigHandler) Login(context.Context, []string) error {
	return scaffolded(h.out, "config slack-auth login")
}

func (h noopConfigHandler) Status(context.Context, []string) error {
	return scaffolded(h.out, "config slack-auth status")
}

func (h noopConfigHandler) Logout(context.Context, []string) error {
	return scaffolded(h.out, "config slack-auth logout")
}

type noopProjectHandler struct {
	out io.Writer
}

func (h noopProjectHandler) ImportRemote(context.Context, []string) error {
	return scaffolded(h.out, "project import-remote")
}

func (h noopProjectHandler) ImportLocal(context.Context, []string) error {
	return scaffolded(h.out, "project import-local")
}

func (h noopProjectHandler) List(context.Context, []string) error {
	return scaffolded(h.out, "project list")
}

func (h noopProjectHandler) Show(context.Context, []string) error {
	return scaffolded(h.out, "project show")
}

func (h noopProjectHandler) Delete(context.Context, []string) error {
	return scaffolded(h.out, "project delete")
}

type noopRuntimeHandler struct {
	out io.Writer
}

func (h noopRuntimeHandler) Start(context.Context, []string) error {
	return scaffolded(h.out, "runtime start")
}

func (h noopRuntimeHandler) Status(context.Context, []string) error {
	return scaffolded(h.out, "runtime status")
}

func (h noopRuntimeHandler) Reload(context.Context, []string) error {
	return scaffolded(h.out, "runtime reload")
}

func (h noopRuntimeHandler) Doctor(context.Context, []string) error {
	return scaffolded(h.out, "runtime doctor")
}

func (h noopRuntimeHandler) Cancel(context.Context, []string) error {
	return scaffolded(h.out, "runtime cancel")
}

func scaffolded(out io.Writer, command string) error {
	if _, err := fmt.Fprintf(out, "%s is scaffolded and ready for follow-up implementation.\n", command); err != nil {
		return err
	}
	return nil
}
