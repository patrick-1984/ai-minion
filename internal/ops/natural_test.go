package ops

import (
	"slices"
	"testing"
)

func TestNaturalCompareOrdersNumbersByValue(t *testing.T) {
	got := []string{"wb-10", "wb-2", "wb-1", "wb-11", "WB-3", "wb-02", "alpha", "wb-1a", "wb-"}
	slices.SortFunc(got, NaturalCompare)
	want := []string{"alpha", "wb-", "wb-1", "wb-1a", "wb-02", "wb-2", "WB-3", "wb-10", "wb-11"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if NaturalCompare("a", "a") != 0 || NaturalCompare("a1", "a01") == 0 {
		t.Fatal("equal names must compare equal and distinct spellings must not")
	}
}
