# Go channel pipelines

A Go pipeline connects stages with channels. Each stage receives values from an
input channel, transforms them, and sends results to an output channel.

Close outbound channels when a stage finishes. Use a cancellation signal or a
context so blocked goroutines can stop when the consumer no longer needs data.
