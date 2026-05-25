//go:build unix

package main

// installer_glue.go — P1a: lookup helper + result senders for the
// skill.install / skill.uninstall dispatchers in loop.go.
//
// The installer rides on the bundle skill (shared rootDir + http
// client + venv heuristics). We can't construct one at agent boot
// because skills.NewBundleSkill is called from main.go and the
// returned Skill is registered globally — getInstaller() looks it
// back up by Kind and wraps it lazily.

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/networkextension/polar-agent/cmd/polar-agent/skills"
)

var (
	installerOnce sync.Once
	installerInst *skills.Installer
)

func getInstaller() *skills.Installer {
	installerOnce.Do(func() {
		s, ok := skills.Default().Get(skills.KindBundle)
		if !ok {
			log.Printf("installer: bundle skill not registered; skill.install frames will be no-op")
			return
		}
		installerInst = skills.NewInstaller(s)
	})
	return installerInst
}

// sendInstallResult serialises an InstallResult into a skill.install.result
// frame and pushes it onto the WS send queue.
func sendInstallResult(send func([]byte) error, kind string, res skills.InstallResult) {
	frame := map[string]any{
		"kind":           kind,
		"install_id":     res.InstallID,
		"status":         string(res.Status),
		"installed_path": res.InstalledPath,
		"error":          res.Error,
		"duration_ms":    res.DurationMS,
		"finished_at":    res.FinishedAt,
		"signed_by":      res.SignedBy, // P2 — empty unless verified
	}
	b, err := json.Marshal(frame)
	if err != nil {
		log.Printf("install.result marshal: %v", err)
		return
	}
	if err := send(b); err != nil {
		log.Printf("install.result send: %v", err)
	}
}

func sendUninstallResult(send func([]byte) error, kind string, res skills.UninstallResult) {
	frame := map[string]any{
		"kind":         kind,
		"install_id":   res.InstallID,
		"status":       string(res.Status),
		"removed_runs": res.RemovedRuns,
		"error":        res.Error,
		"duration_ms":  res.DurationMS,
		"finished_at":  res.FinishedAt,
	}
	b, err := json.Marshal(frame)
	if err != nil {
		log.Printf("uninstall.result marshal: %v", err)
		return
	}
	if err := send(b); err != nil {
		log.Printf("uninstall.result send: %v", err)
	}
}
