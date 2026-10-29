package models

import "testing"

func TestDefaultUserSettingsMatchAppDefaults(t *testing.T) {
	got := DefaultUserSettings()
	if got.Theme != "system" ||
		got.ColorScheme != "dark_teal" ||
		got.Language != "it" ||
		got.FontFamily != "Inter" ||
		got.FontSize != 16 ||
		got.LineSpacing != 1.6 ||
		got.Layout != "split" {
		t.Fatalf("default inattesi: %+v", got)
	}
}
