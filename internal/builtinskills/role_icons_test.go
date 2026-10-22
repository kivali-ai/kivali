package builtinskills_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinskills"
	"github.com/kivali-ai/kivali/internal/roleicon"
)

var update = flag.Bool("update", false, "rewrite the role-icons skill's icon list from internal/roleicon")

// roleIconsHeading opens the part of the role-icons SKILL.md that is
// rendered from roleicon. Everything after it is roleicon.Menu().
const roleIconsHeading = "## The icons\n\n"

// Why this test exists: the role-icons skill is the only place the
// Chief of Staff reads what each icon pictures and suits, and
// propose_hire accepts exactly roleicon's names. An icon added to
// roleicon but not to the file is one the hiring agent never picks; one
// renamed or removed there but still in the file is one it picks and
// has refused. The file checked is the embedded one, which is what
// agents read under /files/skills/. Run with -update to rewrite the
// list after a roleicon change.
func TestRoleIconsSkillListsEveryRoleIcon(t *testing.T) {
	path := filepath.Join("skills", builtinskills.RoleIcons, builtinskills.ManifestName)
	var doc string
	for _, f := range builtinskills.Files(builtinskills.RoleIcons) {
		if f.Path == builtinskills.ManifestName {
			doc = string(f.Data)
		}
	}
	if doc == "" {
		t.Fatalf("%s is not a built-in skill with a %s", builtinskills.RoleIcons, builtinskills.ManifestName)
	}
	head, list, ok := strings.Cut(doc, roleIconsHeading)
	if !ok {
		t.Fatalf("%s has no %q heading for the icon list", path, strings.TrimSpace(roleIconsHeading))
	}
	want := roleicon.Menu()
	if *update {
		if err := os.WriteFile(path, []byte(head+roleIconsHeading+want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if list != want {
		t.Errorf("%s's icon list is not internal/roleicon's; run "+
			"`go test ./internal/builtinskills -run TestRoleIconsSkillListsEveryRoleIcon -update`.\ngot:\n%s\nwant:\n%s",
			path, list, want)
	}
	for _, ic := range roleicon.Icons {
		if !strings.Contains(list, "- "+ic.Name+": ") {
			t.Errorf("%s does not list %q", path, ic.Name)
		}
	}
}
