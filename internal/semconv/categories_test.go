package semconv_test

import (
	"testing"

	"github.com/rknightion/finstore/internal/bankhttp"
	"github.com/rknightion/finstore/internal/semconv"
)

// The bank client's categories are span status descriptions the exporter keeps. A category added
// or renamed in bankhttp must be added here and in semconv, or its spans would read "error".
func TestEveryBankCategoryMayBeAStatusDescription(t *testing.T) {
	for _, c := range []bankhttp.Category{
		bankhttp.CategoryTimeout, bankhttp.CategoryNetwork, bankhttp.CategoryRateLimited,
		bankhttp.CategoryServerError, bankhttp.CategoryUnauthorised,
		bankhttp.CategoryForbiddenInsufficientPermissions, bankhttp.CategoryForbiddenVerificationRequired,
		bankhttp.CategoryBadRequest, bankhttp.CategoryUnexpectedStatus,
	} {
		if !semconv.IsStatusCategory(string(c)) {
			t.Errorf("bankhttp category %q is not a status category", c)
		}
	}
}
