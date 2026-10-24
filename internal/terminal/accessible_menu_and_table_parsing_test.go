package terminal

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func TestAccessibleSelectRenameUsageFailureAndUnknownItem(t *testing.T) {
	setAccessible(t)
	items := []SelectItem{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}}
	cb := &SelectCallbacks{
		OnRename: func(item SelectItem, newName string) (string, error) {
			if item.ID == "b" {
				return "", errors.New("name taken")
			}
			return "", nil
		},
		OnDelete: func(SelectItem) error { return nil },
	}

	input := "r 1\nr 2 new name\nr 9 x\nr 1 renamed\n\n"
	var item SelectItem
	var confirmed bool
	var err error
	out := captureStdout(t, func() {
		item, confirmed, err = accessibleSelect("pick", items, bufio.NewReader(strings.NewReader(input)), cb)
	})
	if err != nil {
		t.Fatalf("accessibleSelect: %v", err)
	}
	if confirmed {
		t.Errorf("cancelled menu reported confirmed item %+v", item)
	}
	for _, want := range []string{
		"usage: r <n> <new name>",
		"rename failed: name taken",
		"no item 9",
		`renamed to renamed`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestAccessibleSelectWithoutCallbacksTreatsRenameAsFilter(t *testing.T) {
	setAccessible(t)
	items := []SelectItem{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}}
	reader := bufio.NewReader(strings.NewReader("r 1 zzz\n*\n1\n"))
	var item SelectItem
	var confirmed bool
	var err error
	captureStdout(t, func() {
		item, confirmed, err = accessibleSelect("pick", items, reader, nil)
	})
	if err != nil || !confirmed {
		t.Fatalf("confirmed=%v err=%v, want the second line to select", confirmed, err)
	}
	if item.ID != "a" {
		t.Errorf("selected %q, want a: \"r 1 zzz\" should filter, not rename", item.ID)
	}
}

func TestAccessibleCommandMenuSelectsSlashCommand(t *testing.T) {
	setAccessible(t)
	var id string
	var err error
	captureStdout(t, func() {
		id, _, err = accessibleCommandMenu(bufio.NewReader(strings.NewReader("1\n")))
	})
	if err != nil {
		t.Fatalf("accessibleCommandMenu: %v", err)
	}
	if id == "" || !strings.HasPrefix(id, "/") {
		t.Errorf("selected id = %q, want a slash command", id)
	}

	captureStdout(t, func() {
		id, _, err = accessibleCommandMenu(bufio.NewReader(strings.NewReader("\n")))
	})
	if err != nil || id != "" {
		t.Errorf("cancelled menu returned %q, %v", id, err)
	}
}

func TestAccessibleSlashCommandsKeepCommandNames(t *testing.T) {
	for _, it := range accessibleSlashCommands() {
		if strings.Contains(it.Text, "Ctrl+") {
			t.Errorf("hint left in %q", it.Text)
		}
		if !strings.HasPrefix(it.Text, it.ID) {
			t.Errorf("text %q does not start with its id %q", it.Text, it.ID)
		}
	}
}

func TestIsSeparatorRowCases(t *testing.T) {
	tests := map[string]bool{
		"|---|---|":       true,
		"| :--- | ---: |": true,
		"|:-:|":           true,
		"|---|":           true,
		"|":               false,
		"| a | b |":       false,
		"|---| x |":       false,
		"|  |":            false,
		"---|---":         false,
		"|-|  |":          false,
	}
	for line, want := range tests {
		if got := isSeparatorRow(line); got != want {
			t.Errorf("isSeparatorRow(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestParseCellsAndAlignment(t *testing.T) {
	if got := parseCells("| a | b |"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("parseCells = %q", got)
	}
	if got := parseCells("a | b"); len(got) != 2 {
		t.Errorf("parseCells without outer pipes = %q", got)
	}
	cases := map[string]Alignment{":---": AlignLeft, "---:": AlignRight, ":--:": AlignCenter, "---": AlignLeft}
	for cell, want := range cases {
		if got := parseAlignment(cell); got != want {
			t.Errorf("parseAlignment(%q) = %v, want %v", cell, got, want)
		}
	}
}

func TestParseMarkdownTableRejectsAndPads(t *testing.T) {
	if _, ok := ParseMarkdownTable([]string{"| a | b |"}); ok {
		t.Error("single line accepted")
	}
	if _, ok := ParseMarkdownTable([]string{"| a | b |", "|---|"}); ok {
		t.Error("header/separator column mismatch accepted")
	}
	tbl, ok := ParseMarkdownTable([]string{
		"| a | b |",
		"|---|---|",
		"| only |",
		"| x | y |",
		"not a row",
		"| after | break |",
	})
	if !ok {
		t.Fatal("valid table rejected")
	}
	if len(tbl.Rows) != 2 {
		t.Fatalf("rows = %v, want the table to stop at the non-row line", tbl.Rows)
	}
	if len(tbl.Rows[0]) != 2 || tbl.Rows[0][0] != "only" || tbl.Rows[0][1] != "" {
		t.Errorf("short row not padded: %q", tbl.Rows[0])
	}
}
