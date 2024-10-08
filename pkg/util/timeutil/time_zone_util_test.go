// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package timeutil

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimeZoneStringToLocation(t *testing.T) {
	aus, err := LoadLocation("Australia/Sydney")
	require.NoError(t, err)

	testCases := []struct {
		tz             string
		std            TimeZoneStringToLocationStandard
		loc            *time.Location
		expectedResult bool
	}{
		{"<+10>-10", TimeZoneStringToLocationISO8601Standard, TimeZoneOffsetToLocation(10 * 60 * 60), true},
		{"<+167>-167", TimeZoneStringToLocationISO8601Standard, TimeZoneOffsetToLocation(167 * 60 * 60), true},
		{"UTC", TimeZoneStringToLocationISO8601Standard, time.UTC, true},
		{"Australia/Sydney", TimeZoneStringToLocationISO8601Standard, aus, true},
		{"<+167>-167", TimeZoneStringToLocationISO8601Standard, TimeZoneOffsetToLocation(167 * 60 * 60), true},
		{"<-167>+167", TimeZoneStringToLocationISO8601Standard, TimeZoneOffsetToLocation(-167 * 60 * 60), true},
		{"<+10>-10", TimeZoneStringToLocationISO8601Standard, TimeZoneOffsetToLocation(10 * 60 * 60), true},
		{"-10", TimeZoneStringToLocationISO8601Standard, TimeZoneOffsetToLocation(-10 * 60 * 60), true},
		{" +10", TimeZoneStringToLocationISO8601Standard, TimeZoneOffsetToLocation(10 * 60 * 60), true},
		{" -10 ", TimeZoneStringToLocationISO8601Standard, TimeZoneOffsetToLocation(-10 * 60 * 60), true},
		{"asdf", TimeZoneStringToLocationISO8601Standard, nil, false},

		{"-10:30", TimeZoneStringToLocationISO8601Standard, FixedTimeZoneOffsetToLocation((10*60*60 + 30*60), "-10:30"), true},
		{"10:30", TimeZoneStringToLocationISO8601Standard, FixedTimeZoneOffsetToLocation(-(10*60*60 + 30*60), "10:30"), true},
		{`GMT-3:00`, TimeZoneStringToLocationISO8601Standard, FixedTimeZoneOffsetToLocation(3*60*60, "GMT-3:00"), true},
		{`GMT+3:00`, TimeZoneStringToLocationISO8601Standard, FixedTimeZoneOffsetToLocation(-3*60*60, "GMT+3:00"), true},
		{`UTC-3:00`, TimeZoneStringToLocationISO8601Standard, FixedTimeZoneOffsetToLocation(3*60*60, "UTC-3:00"), true},
		{`UTC+3:00`, TimeZoneStringToLocationISO8601Standard, FixedTimeZoneOffsetToLocation(-3*60*60, "UTC+3:00"), true},

		{"UTC", TimeZoneStringToLocationPOSIXStandard, time.UTC, true},
		{"Australia/Sydney", TimeZoneStringToLocationPOSIXStandard, aus, true},
		{"<+167>-167", TimeZoneStringToLocationPOSIXStandard, TimeZoneOffsetToLocation(167 * 60 * 60), true},
		{"<-167>+167", TimeZoneStringToLocationPOSIXStandard, TimeZoneOffsetToLocation(-167 * 60 * 60), true},
		{"+10", TimeZoneStringToLocationPOSIXStandard, TimeZoneOffsetToLocation(-10 * 60 * 60), true},
		{"-10", TimeZoneStringToLocationPOSIXStandard, TimeZoneOffsetToLocation(10 * 60 * 60), true},
		{" +10", TimeZoneStringToLocationPOSIXStandard, TimeZoneOffsetToLocation(-10 * 60 * 60), true},
		{" -10", TimeZoneStringToLocationPOSIXStandard, TimeZoneOffsetToLocation(10 * 60 * 60), true},
		{`GMT-3:00`, TimeZoneStringToLocationPOSIXStandard, FixedTimeZoneOffsetToLocation(3*60*60, "GMT-3:00"), true},
		{"asdf", TimeZoneStringToLocationPOSIXStandard, nil, false},
		{"-10:30", TimeZoneStringToLocationPOSIXStandard, FixedTimeZoneOffsetToLocation((10*60*60 + 30*60), "-10:30"), true},
		{"10:30", TimeZoneStringToLocationPOSIXStandard, FixedTimeZoneOffsetToLocation(-(10*60*60 + 30*60), "10:30"), true},
		{`GMT-3:00`, TimeZoneStringToLocationPOSIXStandard, FixedTimeZoneOffsetToLocation(3*60*60, "GMT-3:00"), true},
		{`GMT+3:00`, TimeZoneStringToLocationPOSIXStandard, FixedTimeZoneOffsetToLocation(-3*60*60, "GMT+3:00"), true},
		{`UTC-3:00`, TimeZoneStringToLocationPOSIXStandard, FixedTimeZoneOffsetToLocation(3*60*60, "UTC-3:00"), true},
		{`UTC+3:00`, TimeZoneStringToLocationPOSIXStandard, FixedTimeZoneOffsetToLocation(-3*60*60, "UTC+3:00"), true},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s_%d", tc.tz, tc.std), func(t *testing.T) {
			loc, err := TimeZoneStringToLocation(tc.tz, tc.std)
			if tc.expectedResult {
				assert.NoError(t, err)
				assert.Equal(t, tc.loc, loc)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestTimeZoneOffsetStringConversion(t *testing.T) {
	testCases := []struct {
		timezone   string
		std        TimeZoneStringToLocationStandard
		offsetSecs int64
		ok         bool
	}{
		{`10`, TimeZoneStringToLocationPOSIXStandard, -10 * 60 * 60, true},
		{`-10`, TimeZoneStringToLocationPOSIXStandard, 10 * 60 * 60, true},

		{`10`, TimeZoneStringToLocationISO8601Standard, 10 * 60 * 60, true},
		{`-10`, TimeZoneStringToLocationISO8601Standard, -10 * 60 * 60, true},
		{`10:15`, TimeZoneStringToLocationISO8601Standard, -(10*60*60 + 15*60), true},
		{`+10:15`, TimeZoneStringToLocationISO8601Standard, -(10*60*60 + 15*60), true},
		{`-10:15`, TimeZoneStringToLocationISO8601Standard, (10*60*60 + 15*60), true},
		{`GMT+00:00`, TimeZoneStringToLocationISO8601Standard, 0, true},
		{`UTC-1:00:00`, TimeZoneStringToLocationISO8601Standard, 3600, true},
		{`UTC-1:0:00`, TimeZoneStringToLocationISO8601Standard, 3600, true},
		{`UTC+15:59:0`, TimeZoneStringToLocationISO8601Standard, -57540, true},
		{` GMT +167:59:00  `, TimeZoneStringToLocationISO8601Standard, -604740, true},
		{`GMT-15:59:59`, TimeZoneStringToLocationISO8601Standard, 57599, true},
		{`GMT-06:59`, TimeZoneStringToLocationISO8601Standard, 25140, true},
		{`GMT+167:59:00`, TimeZoneStringToLocationISO8601Standard, -604740, true},
		{`GMT+ 167: 59:0`, TimeZoneStringToLocationISO8601Standard, -604740, true},
		{`GMT+5`, TimeZoneStringToLocationISO8601Standard, -18000, true},
		{`UTC+5:9`, TimeZoneStringToLocationISO8601Standard, -(5*60*60 + 9*60), true},
		{`UTC-5:9:1`, TimeZoneStringToLocationISO8601Standard, (5*60*60 + 9*60 + 1), true},
		{`GMT-15:59:5Z`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT-15:99:1`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT+6:`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT-6:00:`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT+168:00:00`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT-170:00:59`, TimeZoneStringToLocationISO8601Standard, 0, false},
	}

	for i, testCase := range testCases {
		offset, ok := timeZoneOffsetStringConversion(testCase.timezone, testCase.std)
		if offset != testCase.offsetSecs || ok != testCase.ok {
			t.Errorf("%d: Expected offset: %d, success: %v for time %s, but got offset: %d, success: %v",
				i, testCase.offsetSecs, testCase.ok, testCase.timezone, offset, ok)
		}
	}
}
