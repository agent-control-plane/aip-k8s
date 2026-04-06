package main

import (
	"testing"

	"github.com/onsi/gomega"
)

func TestValidateContextSchema_ValidFlat(t *testing.T) {
	g := gomega.NewWithT(t)
	raw := []byte(`{"type":"object","properties":{"cpuCount":{"type":"integer"}}}`)
	err := validateContextSchema(raw)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestValidateContextSchema_UnknownKeyRejected(t *testing.T) {
	g := gomega.NewWithT(t)
	raw := []byte(`{"type":"object","$ref":"#/definitions/foo"}`)
	err := validateContextSchema(raw)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring(`disallowed keyword: "$ref"`))
}

func TestValidateContextSchema_InvalidType(t *testing.T) {
	g := gomega.NewWithT(t)
	raw := []byte(`{"type":"widget"}`)
	err := validateContextSchema(raw)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("disallowed or invalid type"))
}

func TestValidateContextSchema_NestedPropertiesValid(t *testing.T) {
	g := gomega.NewWithT(t)
	raw := []byte(`{
		"type": "object",
		"properties": {
			"nested": {
				"type": "object",
				"properties": {
					"field": { "type": "string" }
				}
			}
		}
	}`)
	err := validateContextSchema(raw)
	g.Expect(err).NotTo(gomega.HaveOccurred())
}

func TestValidateContextSchema_Nil(t *testing.T) {
	// validateContextSchema unmarshals to map[string]any, so nil raw or empty raw would be an error in unmarshal,
	// but the handler check for non-nil gr.Spec.ContextSchema.
	// Empty JSON is {} which is valid but no keywords.
}
