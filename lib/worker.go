package lib

import (
	"fmt"
	"time"
)

type EndpointWorker struct {
	id           int
	busy         bool
	requestQueue chan *Request
	manager      *QueueManager
}

func Worker(w *EndpointWorker, pool *EndpointWorkerPool) {
	var delay int
	if GetCurrentTotalWorkers() > 30 {
		delay = 30
	} else {
		delay = 15
	}

	logger.Debugf("Worker %v waiting for %v ms", w.id, startingWorkers*delay)
	time.Sleep(time.Duration(startingWorkers*delay) * time.Millisecond)

	if startingWorkers > 0 {
		minZero := startingWorkers - 1
		if minZero < 0 {
			minZero = 0
		}

		startingWorkers = minZero
	}

	for request := range w.requestQueue {
		response := NewResponse(request.Channel, request)

		w.manager.DiscordRequestHandler(request, response)

		pool.mu.Lock()
		w.busy = false
		pool.mu.Unlock()

		logger.Debugf("Worker %v is free", w.id)

		pool.mu.Lock()
		select {
		case nextMsg := <-pool.queue:
			pool.mu.Unlock()
			logger.Debugf("Worker %v is still needed, processing next message", w.id)

			pool.mu.Lock()
			w.busy = true
			pool.mu.Unlock()

			go func(req *Request) {
				w.requestQueue <- nextMsg
			}(nextMsg)

			logger.Debugf("Worker %v is processing next message", w.id)

		default:
			pool.mu.Unlock()
			if len(pool.workers) > pool.minWorkers {
				logger.Debugf("Worker %v is not needed, removing worker", w.id)

				pool.mu.Lock()
				for i, worker := range pool.workers {
					if worker == w {
						pool.workers = append(pool.workers[:i], pool.workers[i+1:]...)
						break
					}
				}
				pool.mu.Unlock()
				close(w.requestQueue)

				logger.Debugf("Worker %v removed", w.id)
				WorkerStatus.DeleteLabelValues(pool.name, fmt.Sprint(w.id))
				return
			}

		}
	}
}
