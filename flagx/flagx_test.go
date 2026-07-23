package flagx

import "testing"

func TestEnum(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{"unset uses default", "", "product", false},
		{"valid value", "shadow", "shadow", false},
		{"another valid value", "inventory", "inventory", false},
		{"invalid value", "produkt", "", true},
		{"case sensitive", "Product", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("CHECKOUT_AVAILABILITY_SOURCE", tc.env)
			}
			got, err := Enum("CHECKOUT_AVAILABILITY_SOURCE", "product",
				"product", "shadow", "inventory")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got value %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMustVariantsSuccess(t *testing.T) {
	t.Setenv("ORDER_STOCK_PARTICIPANT", "inventory")
	if got := MustEnum("ORDER_STOCK_PARTICIPANT", "product", "product", "inventory"); got != "inventory" {
		t.Fatalf("MustEnum got %q", got)
	}
	t.Setenv("INVENTORY_READ_SHADOW_PERCENT", "25")
	if got := MustPercent("INVENTORY_READ_SHADOW_PERCENT", 0); got != 25 {
		t.Fatalf("MustPercent got %d", got)
	}
}

func TestEnumInvalidDefault(t *testing.T) {
	if _, err := Enum("FLAGX_TEST_UNSET_VAR", "nope", "a", "b"); err == nil {
		t.Fatal("default outside allowed set must error")
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		def     int
		want    int
		wantErr bool
	}{
		{"unset uses default", "", 10, 10, false},
		{"zero", "0", 10, 0, false},
		{"hundred", "100", 10, 100, false},
		{"over range", "101", 10, 0, true},
		{"negative", "-1", 10, 0, true},
		{"not a number", "ten", 10, 0, true},
		{"default out of range", "", 150, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("INVENTORY_READ_SHADOW_PERCENT", tc.env)
			}
			got, err := Percent("INVENTORY_READ_SHADOW_PERCENT", tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
