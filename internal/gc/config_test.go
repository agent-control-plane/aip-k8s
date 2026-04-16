package gc

import (
	"testing"
	"time"

	"github.com/onsi/gomega"
)

func TestDefaultGCConfig(t *testing.T) {
	t.Run("DefaultGCConfig has safe defaults", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		cfg := DefaultGCConfig()
		gm.Expect(cfg.Enabled).To(gomega.BeFalse())
		gm.Expect(cfg.DryRun).To(gomega.BeTrue())
		gm.Expect(cfg.DiagnosticHardTTL).To(gomega.Equal(14 * 24 * time.Hour))
		gm.Expect(cfg.DiagnosticRetentionTTL).To(gomega.Equal(time.Duration(0)))
		gm.Expect(cfg.ExportType).To(gomega.Equal("none"))
		gm.Expect(cfg.Concurrency).To(gomega.Equal(5))
		gm.Expect(cfg.SafetyMinCount).To(gomega.Equal(10))
	})

	t.Run("Zero DiagnosticRetentionTTL means soft retention disabled", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		cfg := GCConfig{
			DiagnosticRetentionTTL: 0,
			DiagnosticHardTTL:      14 * 24 * time.Hour,
		}
		gm.Expect(cfg.DiagnosticRetentionTTL).To(gomega.Equal(time.Duration(0)))
		gm.Expect(cfg.DiagnosticHardTTL).To(gomega.Equal(14 * 24 * time.Hour))
	})
}
