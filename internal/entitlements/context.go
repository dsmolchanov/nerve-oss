package entitlements

import "context"

type reservationContextKey struct{}

func WithReservation(ctx context.Context, reservation Reservation) context.Context {
	return context.WithValue(ctx, reservationContextKey{}, reservation)
}

func ReservationFromContext(ctx context.Context) (Reservation, bool) {
	res, ok := ctx.Value(reservationContextKey{}).(Reservation)
	return res, ok
}
