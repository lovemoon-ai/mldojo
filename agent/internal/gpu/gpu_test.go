package gpu

import "testing"

func TestParse(t *testing.T) {
	s := parse("0, NVIDIA GeForce RTX 5090, 37, 1234, 32607, 45\n1, NVIDIA H20, [N/A], 10, 97871, 30\n")
	if len(s) != 2 || s[0].Model != "NVIDIA GeForce RTX 5090" || s[0].Util != 37 || s[1].Util != 0 {
		t.Fatalf("%+v", s)
	}
	inv := Inventory(s)
	if inv[0].MemGB != 32 || inv[1].MemGB != 96 {
		t.Fatalf("%+v", inv)
	}
}
