package httpadapter

import (
	"time"

	"github.com/sweetlemon356/lab3-rsoi/internal/gateway/application"
)

type hotelResponse struct {
	HotelUID string `json:"hotelUid"`
	Name     string `json:"name"`
	Country  string `json:"country"`
	City     string `json:"city"`
	Address  string `json:"address"`
	Stars    int    `json:"stars"`
	Price    int64  `json:"price"`
}

type hotelPageResponse struct {
	Page          int             `json:"page"`
	PageSize      int             `json:"pageSize"`
	TotalElements int             `json:"totalElements"`
	Items         []hotelResponse `json:"items"`
}

type hotelInfoResponse struct {
	HotelUID    string `json:"hotelUid"`
	Name        string `json:"name"`
	FullAddress string `json:"fullAddress"`
	Stars       int    `json:"stars"`
}

type paymentResponse struct {
	Status string `json:"status,omitempty"`
	Price  *int64 `json:"price,omitempty"`
}

type loyaltyResponse struct {
	Status           string `json:"status,omitempty"`
	Discount         *int   `json:"discount,omitempty"`
	ReservationCount *int   `json:"reservationCount,omitempty"`
}

type reservationResponse struct {
	ReservationUID string            `json:"reservationUid"`
	Hotel          hotelInfoResponse `json:"hotel"`
	StartDate      string            `json:"startDate"`
	EndDate        string            `json:"endDate"`
	Status         string            `json:"status"`
	Payment        paymentResponse   `json:"payment"`
}

type userInfoResponse struct {
	Reservations []reservationResponse `json:"reservations"`
	Loyalty      loyaltyResponse       `json:"loyalty"`
}

type createReservationRequest struct {
	HotelUID  string `json:"hotelUid"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
}

type createReservationResponse struct {
	ReservationUID string          `json:"reservationUid"`
	HotelUID       string          `json:"hotelUid"`
	StartDate      string          `json:"startDate"`
	EndDate        string          `json:"endDate"`
	Discount       int             `json:"discount"`
	Status         string          `json:"status"`
	Payment        paymentResponse `json:"payment"`
}

func reservationResponses(views []application.ReservationView) []reservationResponse {
	responses := make([]reservationResponse, 0, len(views))
	for _, view := range views {
		responses = append(responses, reservationJSON(view))
	}
	return responses
}

func reservationJSON(view application.ReservationView) reservationResponse {
	return reservationResponse{
		ReservationUID: view.ReservationUID,
		Hotel: hotelInfoResponse{
			HotelUID:    view.Hotel.HotelUID,
			Name:        view.Hotel.Name,
			FullAddress: view.Hotel.FullAddress,
			Stars:       view.Hotel.Stars,
		},
		StartDate: view.StartDate.Format(time.DateOnly),
		EndDate:   view.EndDate.Format(time.DateOnly),
		Status:    view.Status,
		Payment:   paymentJSON(view.Payment, view.PaymentUnavailable),
	}
}

func paymentJSON(info application.PaymentInfo, missing bool) paymentResponse {
	if missing {
		return paymentResponse{}
	}
	return paymentResponse{Status: info.Status, Price: &info.Price}
}

func loyaltyJSON(info application.LoyaltyInfo, missing ...bool) loyaltyResponse {
	if len(missing) > 0 && missing[0] {
		return loyaltyResponse{}
	}
	return loyaltyResponse{Status: info.Status, Discount: &info.Discount, ReservationCount: &info.ReservationCount}
}
