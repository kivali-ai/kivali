package store

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// One connection per installation. Records stored while an integration
// (Git) could hold several connections, each with a label, and access
// rows that named theirs by connection_id, are brought to that shape at
// boot and after a restore, without deleting anything.

// retiredConnectionsDir is where a connection set aside is kept, beside
// connections/ under the integration's directory, as it was stored. Its
// Secret is kept too, and nothing reads either.
const retiredConnectionsDir = "connections-retired"

// legacyConnection is what MigrateIntegrationConnections reads of a
// connection record beyond IntegrationConnection: the label the owner
// gave it.
type legacyConnection struct {
	Label  string            `yaml:"label"`
	Fields map[string]string `yaml:"fields"`
}

// legacyAccess is what MigrateIntegrationConnections reads of an access
// row beyond IntegrationAccess: the connection it was set for.
type legacyAccess struct {
	ConnectionID string `yaml:"connection_id"`
}

// retiredAccess is an access row as MigrateIntegrationConnections
// writes one it sets aside: the row, and the connection it was set for,
// kept.
type retiredAccess struct {
	IntegrationAccess `yaml:",inline"`
	ConnectionID      string `yaml:"connection_id,omitempty"`
}

// MigrateIntegrationConnections gives every integration one connection.
// It is idempotent, and runs at every boot and after a restore, after
// InstallBuiltinIntegrations. For each integration:
//
//   - With several connection records, the one most access rows name is
//     kept (ties: the oldest). Each other record moves to
//     connections-retired/<id>.yaml as it is, and its Secret stays.
//   - An access row that names its connection by connection_id loses
//     the field when that is the connection kept. A row naming any
//     other connection is set aside (IntegrationAccess.Retired): it
//     grants nothing, and says which connection it was set for and to
//     set it again. Its connection_id stays on disk.
//
// Nothing is deleted. What it did is logged.
func (s *FSStore) MigrateIntegrationConnections() error {
	its, err := s.ListIntegrations()
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range its {
		if err := s.migrateIntegrationConnections(it); err != nil {
			errs = append(errs, fmt.Errorf("integration %s: one connection: %w", it.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *FSStore) migrateIntegrationConnections(it Integration) error {
	unlock := s.lockIntegration(it.Name)
	defer unlock()
	dir, err := s.integrationDir(it.Name)
	if err != nil {
		return err
	}
	conns, err := s.ListIntegrationConnections(it.Name)
	if err != nil {
		return err
	}
	rows, err := s.readIntegrationAccessRows(it.Name)
	if err != nil {
		return err
	}
	rowConn := map[string]string{}
	for _, row := range rows {
		var legacy legacyAccess
		if err := readYAMLFile(filepath.Join(dir, "access", row.Slug+".yaml"), &legacy); err != nil {
			return fmt.Errorf("access %s: %w", row.Slug, err)
		}
		if legacy.ConnectionID != "" {
			rowConn[row.Slug] = legacy.ConnectionID
		}
	}

	kept := ""
	if len(conns) > 0 {
		// The order is oldest first, so a tie keeps the oldest.
		count := map[string]int{}
		for _, id := range rowConn {
			count[id]++
		}
		kept = conns[0].ID
		for _, c := range conns[1:] {
			if count[c.ID] > count[kept] {
				kept = c.ID
			}
		}
	}
	// The records move first: should this stop half way, the next boot
	// finds the one kept, and the rows still name their connections.
	var retired []string
	for _, c := range conns {
		if c.ID == kept {
			continue
		}
		if err := os.MkdirAll(filepath.Join(dir, retiredConnectionsDir), 0o755); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, "connections", c.ID+".yaml"), filepath.Join(dir, retiredConnectionsDir, c.ID+".yaml")); err != nil {
			return err
		}
		retired = append(retired, c.ID)
	}

	var setAside []string
	for _, row := range rows {
		id, ok := rowConn[row.Slug]
		if !ok || row.Retired != "" {
			continue
		}
		path := filepath.Join(dir, "access", row.Slug+".yaml")
		if id == kept {
			if err := writeYAMLFile(path, row); err != nil {
				return err
			}
			continue
		}
		row.Retired = s.retiredAccessReason(dir, it, id)
		if err := writeYAMLFile(path, retiredAccess{IntegrationAccess: row, ConnectionID: id}); err != nil {
			return err
		}
		setAside = append(setAside, row.Slug)
	}

	if len(retired) > 0 || len(setAside) > 0 {
		sort.Strings(setAside)
		log.Printf("integration %s: one connection per installation: kept connection %s, set aside %s; access to set again: %s",
			it.Name, cmp.Or(kept, "none"), wordsOrNone(retired), wordsOrNone(setAside))
	}
	return nil
}

// retiredAccessReason is why a row set for the connection id grants
// nothing: the connection named by its label, else its host, when its
// record is one set aside.
func (s *FSStore) retiredAccessReason(dir string, it Integration, id string) string {
	var c legacyConnection
	if !validFileName(id) || readYAMLFile(filepath.Join(dir, retiredConnectionsDir, id+".yaml"), &c) != nil {
		return "it was set for a connection that is no longer there; set it again"
	}
	name := cmp.Or(strings.TrimSpace(c.Label), strings.TrimSpace(c.Fields["host"]), id)
	return fmt.Sprintf("set for the connection %s, which %s no longer has; set it again", name, cmp.Or(it.DisplayName, it.Name))
}

func wordsOrNone(ss []string) string {
	if len(ss) == 0 {
		return "none"
	}
	return strings.Join(ss, ", ")
}

// RetiredIntegrationConnections returns the connection records
// MigrateIntegrationConnections set aside, by id; none is not an error.
func (s *FSStore) RetiredIntegrationConnections(name string) ([]IntegrationConnection, error) {
	dir, err := s.integrationDir(name)
	if err != nil {
		return nil, ErrNotFound
	}
	entries, err := os.ReadDir(filepath.Join(dir, retiredConnectionsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []IntegrationConnection
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() || !validFileName(id) {
			continue
		}
		var c IntegrationConnection
		if err := readYAMLFile(filepath.Join(dir, retiredConnectionsDir, e.Name()), &c); err != nil {
			return nil, fmt.Errorf("connection %s: %w", id, err)
		}
		c.ID = id
		out = append(out, c)
	}
	return out, nil
}
