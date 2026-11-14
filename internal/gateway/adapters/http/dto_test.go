package httpadapter

import (
	"encoding/json"
	app "github.com/sweetlemon356/lab3-rsoi/internal/gateway/application"
	"strings"
	"testing"
	"time"
)

func TestFallbackObjectsAreEmptyAndZeroValuesRemainPresent(t *testing.T) {
	view := app.ReservationView{ReservationUID: "booking", Hotel: app.HotelInfo{HotelUID: "hotel"}, StartDate: time.Date(2021, 10, 8, 0, 0, 0, 0, time.UTC), EndDate: time.Date(2021, 10, 11, 0, 0, 0, 0, time.UTC), Status: "PAID", PaymentUnavailable: true}
	encoded, err := json.Marshal(reservationJSON(view))
	if err != nil || !strings.Contains(string(encoded), `"payment":{}`) {
		t.Fatalf("json=%s err=%v", encoded, err)
	}
	encoded, err = json.Marshal(loyaltyJSON(app.LoyaltyInfo{}, true))
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("fallback=%s err=%v", encoded, err)
	}
	encoded, err = json.Marshal(paymentJSON(app.PaymentInfo{Status: "PAID", Price: 0}, false))
	if err != nil || !strings.Contains(string(encoded), `"price":0`) {
		t.Fatalf("zero payment=%s err=%v", encoded, err)
	}
	encoded, err = json.Marshal(loyaltyJSON(app.LoyaltyInfo{Status: "BRONZE", Discount: 5, ReservationCount: 0}))
	if err != nil || !strings.Contains(string(encoded), `"reservationCount":0`) {
		t.Fatalf("zero count=%s err=%v", encoded, err)
	}
}
