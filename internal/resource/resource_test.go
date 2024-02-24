package resource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutf/tofutf/internal"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name         string
		resourceName *string
		want         error
	}{
		{"nil", nil, internal.ErrRequiredName},
		{"dot", internal.String("."), internal.ErrInvalidName},
		{"underscore", internal.String("_"), nil},
		{"acme-corp", internal.String("acme-corp"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.resourceName)
			assert.Equal(t, tt.want, err)
		})
	}
}
