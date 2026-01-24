package main

func flushChannel[T any](c chan T) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}
