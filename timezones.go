package main

import (
	"fmt"
	"time"
)

// TimezoneOption is one entry in the Settings timezone dropdown.
type TimezoneOption struct {
	Value  string // IANA name or "Local"
	Label  string // Human label with current offset, e.g. "Asia/Shanghai (UTC+08:00)"
	Custom bool   // True when a stored value did not match the curated list
}

// TimezoneGroup is a region-labelled cluster of options for <optgroup>.
type TimezoneGroup struct {
	Label   string
	Options []TimezoneOption
}

// curatedTimezones is the short list exposed in the Settings dropdown. ~40
// major IANA zones plus Local and UTC. Tzdata is linked into the binary via
// the `time` stdlib; no external list needs to be shipped.
var curatedTimezones = []TimezoneGroup{
	{
		Label: "Default",
		Options: []TimezoneOption{
			{Value: "Local", Label: "Local (server timezone)"},
			{Value: "UTC", Label: "UTC"},
		},
	},
	{
		Label: "Americas",
		Options: []TimezoneOption{
			{Value: "America/New_York"},
			{Value: "America/Chicago"},
			{Value: "America/Denver"},
			{Value: "America/Los_Angeles"},
			{Value: "America/Toronto"},
			{Value: "America/Mexico_City"},
			{Value: "America/Sao_Paulo"},
			{Value: "America/Argentina/Buenos_Aires"},
		},
	},
	{
		Label: "Europe & Africa",
		Options: []TimezoneOption{
			{Value: "Europe/London"},
			{Value: "Europe/Berlin"},
			{Value: "Europe/Paris"},
			{Value: "Europe/Madrid"},
			{Value: "Europe/Rome"},
			{Value: "Europe/Amsterdam"},
			{Value: "Europe/Zurich"},
			{Value: "Europe/Moscow"},
			{Value: "Europe/Istanbul"},
			{Value: "Africa/Cairo"},
			{Value: "Africa/Johannesburg"},
		},
	},
	{
		Label: "Asia",
		Options: []TimezoneOption{
			{Value: "Asia/Dubai"},
			{Value: "Asia/Tehran"},
			{Value: "Asia/Karachi"},
			{Value: "Asia/Kolkata"},
			{Value: "Asia/Dhaka"},
			{Value: "Asia/Bangkok"},
			{Value: "Asia/Jakarta"},
			{Value: "Asia/Singapore"},
			{Value: "Asia/Hong_Kong"},
			{Value: "Asia/Shanghai"},
			{Value: "Asia/Taipei"},
			{Value: "Asia/Tokyo"},
			{Value: "Asia/Seoul"},
		},
	},
	{
		Label: "Oceania",
		Options: []TimezoneOption{
			{Value: "Australia/Perth"},
			{Value: "Australia/Sydney"},
			{Value: "Pacific/Auckland"},
		},
	},
}

// TimezoneGroupsWithOffsets returns the curated timezone groups with each
// option's Label filled in ("Asia/Shanghai (UTC+08:00)"). If the currently
// stored value is not present in the curated list, it is appended to a new
// "Custom" group at the top so the user does not lose their choice.
func TimezoneGroupsWithOffsets(selected string) []TimezoneGroup {
	// Build a copy we can mutate without touching the package-level literal.
	out := make([]TimezoneGroup, 0, len(curatedTimezones)+1)
	found := false
	for _, g := range curatedTimezones {
		ng := TimezoneGroup{Label: g.Label, Options: make([]TimezoneOption, 0, len(g.Options))}
		for _, o := range g.Options {
			opt := o
			if opt.Value == selected {
				found = true
			}
			opt.Label = formatTimezoneLabel(opt.Value, opt.Label)
			ng.Options = append(ng.Options, opt)
		}
		out = append(out, ng)
	}
	if !found && selected != "" {
		custom := TimezoneOption{
			Value:  selected,
			Label:  formatTimezoneLabel(selected, "") + " (custom)",
			Custom: true,
		}
		out = append([]TimezoneGroup{{Label: "Custom", Options: []TimezoneOption{custom}}}, out...)
	}
	return out
}

// formatTimezoneLabel produces "Asia/Shanghai (UTC+08:00)" when the zone
// loads successfully. Falls back to the raw value when time.LoadLocation
// can't parse it (unlikely for curated entries, common for legacy typos).
// baseLabel, if set, is kept as-is and only the offset is appended.
func formatTimezoneLabel(value, baseLabel string) string {
	label := baseLabel
	if label == "" {
		label = value
	}
	loc, err := time.LoadLocation(value)
	if err != nil {
		return label
	}
	_, offset := time.Now().In(loc).Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	hours := offset / 3600
	mins := (offset % 3600) / 60
	return fmt.Sprintf("%s (UTC%s%02d:%02d)", label, sign, hours, mins)
}
