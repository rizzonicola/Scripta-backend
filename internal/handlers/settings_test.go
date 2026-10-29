package handlers

import (
	"testing"

	"notes-server/internal/models"
)

func TestValidateSettingsFillsAppDefaults(t *testing.T) {
	var got models.UserSettings
	if err := validateSettings(&got); err != nil {
		t.Fatalf("validateSettings: %v", err)
	}
	want := models.DefaultUserSettings()
	if got.Theme != want.Theme ||
		got.ColorScheme != want.ColorScheme ||
		got.Language != want.Language ||
		got.FontFamily != want.FontFamily ||
		got.FontSize != want.FontSize ||
		got.LineSpacing != want.LineSpacing ||
		got.Layout != want.Layout {
		t.Fatalf("fallback inattesi: %+v, attesi %+v", got, want)
	}
}

func TestValidateSettingsRejectsUnknownEnums(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*models.UserSettings)
	}{
		{"theme", func(s *models.UserSettings) { s.Theme = "neon" }},
		{"color_scheme", func(s *models.UserSettings) { s.ColorScheme = "default" }},
		{"language", func(s *models.UserSettings) { s.Language = "de" }},
		{"font_family", func(s *models.UserSettings) { s.FontFamily = "Comic Sans" }},
		{"layout", func(s *models.UserSettings) { s.Layout = "reader" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			settings := models.DefaultUserSettings()
			tc.mutate(&settings)
			if err := validateSettings(&settings); err == nil {
				t.Fatalf("%s non validato: %+v", tc.name, settings)
			}
		})
	}
}

func TestValidateSettingsAcceptsSupportedEnums(t *testing.T) {
	for _, theme := range []string{"light", "dark", "system"} {
		for _, scheme := range []string{"dark_teal", "oled", "nord", "midnight_purple", "forest", "coffee", "clean_light", "solarized_light"} {
			for _, language := range []string{"it", "en", "fr", "system"} {
				for _, font := range []string{"Inter", "JetBrains Mono"} {
					for _, layout := range []string{"single_pane", "split", "grid"} {
						settings := models.UserSettings{
							Theme: theme, ColorScheme: scheme, Language: language,
							FontFamily: font, FontSize: 16, LineSpacing: 1.6, Layout: layout,
						}
						if err := validateSettings(&settings); err != nil {
							t.Fatalf("combinazione rifiutata (%s/%s/%s/%s/%s): %v", theme, scheme, language, font, layout, err)
						}
					}
				}
			}
		}
	}
}
