package shared

import (
	"errors"
	"testing"
)

type messageBufferWriter struct {
	expect          Expect
	successCalled   *bool
	errorCalled     *bool
	returnError     bool
	returnWrongSize bool
}

type mockFormatter struct {
	message string
}

func (writer messageBufferWriter) Write(data []byte) (int, error) {
	defer func() {
		*writer.successCalled = false
		*writer.errorCalled = false
	}()

	if writer.returnWrongSize {
		return 0, nil
	}

	if writer.returnError {
		return len(data), errors.New("test")
	}

	return len(data), nil
}

func (writer messageBufferWriter) onSuccess() bool {
	*writer.successCalled = true
	return true
}

func (writer messageBufferWriter) onError(err error) {
	*writer.errorCalled = true
}

func (mock *mockFormatter) PrepareMessage(msg Message) {
	mock.message = string(msg.Data)
}

func (mock *mockFormatter) GetLength() int {
	return len(mock.message)
}

func (mock *mockFormatter) String() string {
	return mock.message
}

func (mock *mockFormatter) CopyTo(dest []byte) int {
	return copy(dest, []byte(mock.message))
}

func TestMessageBuffer(t *testing.T) {
	expect := NewExpect(t)
	writer := messageBufferWriter{expect, new(bool), new(bool), false, false}

	test10 := NewMessage("1234567890", []MessageStreamID{WildcardStreamID}, 0)
	test20 := NewMessage("12345678901234567890", []MessageStreamID{WildcardStreamID}, 1)
	buffer := NewMessageBuffer(15, new(mockFormatter))

	// Test optionals

	buffer.Flush(writer, nil, nil)
	buffer.WaitForFlush()

	// Test empty flush

	buffer.Flush(writer, writer.onSuccess, writer.onError)
	buffer.WaitForFlush()

	expect.False(*writer.successCalled)
	expect.False(*writer.errorCalled)

	// Test regular appends

	result := buffer.Append(test10)
	expect.True(result)

	result = buffer.Append(test10)
	expect.False(result) // too large

	buffer.Flush(writer, writer.onSuccess, writer.onError)
	buffer.WaitForFlush()

	expect.True(*writer.successCalled)
	expect.False(*writer.errorCalled)

	// Test oversize append

	result = buffer.Append(test20)
	expect.True(result) // Too large -> ignored

	// Test writer error

	result = buffer.Append(test10)
	expect.True(result)

	writer.returnError = true
	buffer.Flush(writer, writer.onSuccess, writer.onError)
	buffer.WaitForFlush()

	expect.False(*writer.successCalled)
	expect.True(*writer.errorCalled)

	// Test writer size mismatch

	writer.returnWrongSize = true
	buffer.Flush(writer, writer.onSuccess, writer.onError)
	buffer.WaitForFlush()

	expect.False(*writer.successCalled)
	expect.True(*writer.errorCalled)
}