package common

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestFormatTokensAndCostPrefixesEstimatedUsage(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()

	rendered := formatTokensAndCost(&sty, 120, 1000, 0, true)
	actual := ansi.Strip(rendered)

	require.Contains(t, actual, "~12%")
	require.Contains(t, actual, "(120)")
	require.Contains(t, actual, "$0.00")
	require.True(t, strings.Contains(rendered, sty.ModelInfo.TokenPercentage.Render("~12%")))
}

func TestFormatTokensAndCostOmitsEstimatedPrefix(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()

	actual := ansi.Strip(formatTokensAndCost(&sty, 120, 1000, 0, false))

	require.Contains(t, actual, "12%")
	require.NotContains(t, actual, "~12%")
}

func TestFormatCacheUsageShowsHitRateAndSplit(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()

	// A healthy turn: nearly the whole prompt came back from the cache.
	actual := ansi.Strip(formatCacheUsage(&sty, 33_876, 0, 33_878))
	require.Contains(t, actual, "99%")
	require.Contains(t, actual, "33.9K read")
	require.Contains(t, actual, "0 write")

	// The expensive turn this readout exists to expose: nothing read, the
	// entire prefix rewritten.
	actual = ansi.Strip(formatCacheUsage(&sty, 0, 51_461, 51_463))
	require.Contains(t, actual, "0%")
	require.Contains(t, actual, "51.5K write")
}

func TestFormatCacheUsageEmptyWithoutCacheActivity(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()

	require.Empty(t, formatCacheUsage(&sty, 0, 0, 1_200))
}
