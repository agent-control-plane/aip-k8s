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
		gm.Expect(cfg.HardTTL).To(gomega.Equal(time.Duration(0)))
		gm.Expect(cfg.SafetyMinCount).To(gomega.Equal(10))
	})

	t.Run("Zero HardTTL disables AgentRequest GC", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		cfg := GCConfig{Enabled: true, HardTTL: 0}
		gm.Expect(cfg.AgentRequestGCEnabled()).To(gomega.BeFalse())
	})

	t.Run("Positive HardTTL with Enabled=true enables AgentRequest GC", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		cfg := GCConfig{Enabled: true, HardTTL: time.Hour}
		gm.Expect(cfg.AgentRequestGCEnabled()).To(gomega.BeTrue())
	})

	t.Run("Enabled=false disables AgentRequest GC even with positive HardTTL", func(t *testing.T) {
		gm := gomega.NewWithT(t)
		cfg := GCConfig{Enabled: false, HardTTL: time.Hour}
		gm.Expect(cfg.AgentRequestGCEnabled()).To(gomega.BeFalse())
	})
}
