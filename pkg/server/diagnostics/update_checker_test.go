// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diagnostics_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/diagnostics"
	"github.com/cockroachdb/cockroach/pkg/testutils/diagutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/cloudinfo"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestCheckVersion(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer cloudinfo.Disable()()

	ctx := context.Background()

	t.Run("expected-reporting", func(t *testing.T) {
		r := diagutils.NewServer()
		defer r.Close()

		url := r.URL()
		s := serverutils.StartServerOnly(t, base.TestServerArgs{
			Knobs: base.TestingKnobs{
				Server: &server.TestingKnobs{
					DiagnosticsTestingKnobs: diagnostics.TestingKnobs{
						OverrideUpdatesURL: &url,
					},
				},
			},
		})
		defer s.Stopper().Stop(ctx)
		s.UpdateChecker().(*diagnostics.UpdateChecker).CheckForUpdates(ctx)
		r.Close()

		require.Equal(t, 1, r.NumRequests())

		last := r.LastRequestData()
		require.Equal(t, s.StorageLayer().StorageClusterID().String(), last.UUID)
		require.Equal(t, "system", last.TenantID)
		require.Equal(t, build.GetInfo().Tag, last.Version)
		require.Equal(t, "OSS", last.LicenseType)
		require.Equal(t, "false", last.Internal)
	})

	t.Run("npe", func(t *testing.T) {
		// Ensure nil, which happens when an empty env override URL is used, does not
		// cause a crash.
		var nilURL *url.URL
		s := serverutils.StartServerOnly(t, base.TestServerArgs{
			Knobs: base.TestingKnobs{
				Server: &server.TestingKnobs{
					DiagnosticsTestingKnobs: diagnostics.TestingKnobs{
						OverrideUpdatesURL:   &nilURL,
						OverrideReportingURL: &nilURL,
					},
				},
			},
		})
		defer s.Stopper().Stop(ctx)
		s.UpdateChecker().(*diagnostics.UpdateChecker).CheckForUpdates(ctx)
		s.DiagnosticsReporter().(*diagnostics.Reporter).ReportDiagnostics(ctx)
	})
}
