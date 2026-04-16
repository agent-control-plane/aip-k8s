package gc

import (
	"context"

	governancev1alpha1 "github.com/ravisantoshgudimetla/aip-k8s/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testDiag(name string) *governancev1alpha1.AgentDiagnostic {
	return &governancev1alpha1.AgentDiagnostic{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
}

type mockExporter struct {
	exportFn func(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error
}

func (m *mockExporter) Export(ctx context.Context, obj *governancev1alpha1.AgentDiagnostic) error {
	return m.exportFn(ctx, obj)
}
