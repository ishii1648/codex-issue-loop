package supervisor

import (
	"math/rand"
	"time"
)

const (
	backoffInitial = 5 * time.Second
	backoffMaximum = 5 * time.Minute
	backoffJitter  = 0.20
)

type Clock interface {
	Now() time.Time
}

type RandomSource interface {
	Float64() float64
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type systemRandom struct{}

func (systemRandom) Float64() float64 { return rand.Float64() }

func (l *Loop) now() time.Time {
	clock := l.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return clock.Now().UTC()
}

func (l *Loop) retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := backoffInitial
	for index := 1; index < attempt && delay < backoffMaximum; index++ {
		delay *= 2
	}
	if delay > backoffMaximum {
		delay = backoffMaximum
	}
	return l.jitter(delay, backoffJitter, backoffMaximum)
}

func (l *Loop) pollDelay(base time.Duration) time.Duration {
	return l.jitter(base, backoffJitter, 0)
}

func (l *Loop) jitter(base time.Duration, ratio float64, maximum time.Duration) time.Duration {
	random := l.Random
	if random == nil {
		random = systemRandom{}
	}
	value := random.Float64()
	if value < 0 {
		value = 0
	}
	if value > 1 {
		value = 1
	}
	factor := 1 - ratio + (2 * ratio * value)
	delay := time.Duration(float64(base) * factor)
	if maximum > 0 && delay > maximum {
		return maximum
	}
	return delay
}
