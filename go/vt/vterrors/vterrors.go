/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package vterrors provides simple error handling primitives for Vitess
//
// In all Vitess code, errors should be propagated using vterrors.Wrapf()
// and not fmt.Errorf(). This makes sure that stacktraces are kept and
// propagated correctly.
//
// # New errors should be created using vterrors.New or vterrors.Errorf
//
// Vitess uses canonical error codes for error reporting. This is based
// on years of industry experience with error reporting. This idea is
// that errors should be classified into a small set of errors (10 or so)
// with very specific meaning. Each error has a code, and a message. When
// errors are passed around (even through RPCs), the code is
// propagated. To handle errors, only the code should be looked at (and
// not string-matching on the error message).
//
// Error codes are defined in /proto/vtrpc.proto. Along with an
// RPCError message that can be used to transmit errors through RPCs, in
// the message payloads. These codes match the names and numbers defined
// by gRPC.
//
// A standardized error implementation that allows you to build an error
// with an associated canonical code is also defined.
// While sending an error through gRPC, these codes are transmitted
// using gRPC's error propagation mechanism and decoded back to
// the original code on the other end.
//
// # Retrieving the cause of an error
//
// Using vterrors.Wrap constructs a stack of errors, adding context to the
// preceding error, instead of simply building up a string.
// Depending on the nature of the error it may be necessary to reverse the
// operation of errors.Wrap to retrieve the original error for inspection.
// Any error value which implements this interface
//
//	type causer interface {
//	        Cause() error
//	}
//
// can be inspected by vterrors.Cause and vterrors.RootCause.
//
//   - vterrors.Cause will find the immediate cause if one is available, or nil
//     if the error is not a `causer` or if no cause is available.
//
//   - vterrors.RootCause will recursively retrieve
//     the topmost error which does not implement causer, which is assumed to be
//     the original cause. For example:
//
//     switch err := errors.RootCause(err).(type) {
//     case *MyError:
//     // handle specifically
//     default:
//     // unknown error
//     }
//
// causer interface is not exported by this package, but is considered a part
// of stable public API.
//
// # Formatted printing of errors
//
// All error values returned from this package implement fmt.Formatter and can
// be formatted by the fmt package. The following verbs are supported
//
//	%s    print the error. If the error has a Cause it will be
//	      printed recursively
//	%v    extended format. Each Frame of the error's StackTrace will
//	      be printed in detail.
//
// Most but not all of the code in this file was originally copied from
// https://github.com/pkg/errors/blob/v0.8.0/errors.go
package vterrors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/spf13/pflag"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/utils"
)

// logErrStacks controls whether printing errors includes the
// embedded stack trace in the output.
var logErrStacks bool
var muLogErrStacks sync.Mutex

func getLogErrStacks() bool {
	muLogErrStacks.Lock()
	defer muLogErrStacks.Unlock()
	return logErrStacks
}

func setLogErrStacks(val bool) {
	muLogErrStacks.Lock()
	defer muLogErrStacks.Unlock()
	logErrStacks = val
}

// RegisterFlags registers the command-line options that control vterror
// behavior on the provided FlagSet.
func RegisterFlags(fs *pflag.FlagSet) {
	muLogErrStacks.Lock()
	defer muLogErrStacks.Unlock()
	utils.SetFlagBoolVar(fs, &logErrStacks, "log-err-stacks", false, "log stack traces for errors")
}

// New returns an error with the supplied message.
// New also records the stack trace at the point it was called.
func New(code vtrpcpb.Code, message string) error {
	return &fundamental{
		msg:   message,
		code:  code,
		stack: callers(),
	}
}

// Errorf formats according to a format specifier and returns the string
// as a value that satisfies error.
// Errorf also records the stack trace at the point it was called.
// Use this for Vitess-specific errors that don't have a MySQL counterpart
func Errorf(code vtrpcpb.Code, format string, args ...any) error {
	return &fundamental{
		msg:   fmt.Sprintf(format, args...),
		code:  code,
		stack: callers(),
	}
}

// NewErrorf formats according to a format specifier and returns the string
// as a value that satisfies error.
// NewErrorf also records the stack trace at the point it was called.
// Use this for errors in Vitess that we eventually want to mimic as a MySQL error
func NewErrorf(code vtrpcpb.Code, state State, format string, args ...any) error {
	return NewError(code, state, fmt.Sprintf(format, args...))
}

// NewErrorf formats according to a format specifier and returns the string
// as a value that satisfies error.
// NewErrorf also records the stack trace at the point it was called.
// Use this for errors in Vitess that we eventually want to mimic as a MySQL error
func NewError(code vtrpcpb.Code, state State, msg string) error {
	return &fundamental{
		msg:   msg,
		code:  code,
		state: state,
		stack: callers(),
	}
}

// fundamental is an error that has a message and a stack, but no caller.
type fundamental struct {
	msg   string
	code  vtrpcpb.Code
	state State
	*stack
}

func (f *fundamental) Error() string { return f.msg }

func (f *fundamental) Format(s fmt.State, verb rune) {
	switch verb {
	case 'v':
		panicIfError(io.WriteString(s, "Code: "+f.code.String()+"\n"))
		panicIfError(io.WriteString(s, f.msg+"\n"))
		if getLogErrStacks() {
			f.stack.Format(s, verb)
		}
		return
	case 's':
		panicIfError(io.WriteString(s, f.msg))
	case 'q':
		panicIfError(fmt.Fprintf(s, "%q", f.msg))
	}
}

// Code returns the error code if it's a vtError.
// If err is nil, it returns ok.
func Code(err error) vtrpcpb.Code {
	if err == nil {
		return vtrpcpb.Code_OK
	}
	if err, ok := err.(ErrorWithCode); ok {
		return err.ErrorCode()
	}

	cause := Cause(err)
	if cause != err && cause != nil {
		// If we did not find an error code at the outer level, let's find the cause and check it's code
		return Code(cause)
	}

	// Handle some special cases.
	switch err {
	case context.Canceled:
		return vtrpcpb.Code_CANCELED
	case context.DeadlineExceeded:
		return vtrpcpb.Code_DEADLINE_EXCEEDED
	}
	return vtrpcpb.Code_UNKNOWN
}

// ErrState returns the error state if it's a vtError.
// If err is nil, it returns Undefined.
func ErrState(err error) State {
	if err == nil {
		return Undefined
	}

	if err, ok := err.(ErrorWithState); ok {
		return err.ErrorState()
	}

	cause := Cause(err)
	if cause != err && cause != nil {
		// If we did not find an error state at the outer level, let's find the cause and check it's state
		return ErrState(cause)
	}
	return Undefined
}

// Wrap returns an error annotating err with a stack trace
// at the point Wrap is called, and the supplied message.
// If err is nil, Wrap returns nil.
func Wrap(err error, message string) error {
	if err == nil {
		return nil
	}
	return &wrapping{
		cause: err,
		msg:   message,
		stack: callers(),
	}
}

// Wrapf returns an error annotating err with a stack trace
// at the point Wrapf is call, and the format specifier.
// If err is nil, Wrapf returns nil.
func Wrapf(err error, format string, args ...any) error {
	return Wrap(err, fmt.Sprintf(format, args...))
}

// Unwrap attempts to return the Cause of the given error, if it is indeed the result of a vterrors.Wrapf()
// The function indicates whether the error was indeed wrapped. If the error was not wrapped, the function
// returns the original error.
func Unwrap(err error) (wasWrapped bool, unwrapped error) {
	var w *wrapping
	if errors.As(err, &w) {
		return true, w.Cause()
	}
	return false, err
}

// UnwrapAll attempts to recursively unwrap the given error, and returns the most underlying cause
func UnwrapAll(err error) error {
	wasWrapped := true
	for wasWrapped {
		wasWrapped, err = Unwrap(err)
	}
	return err
}

type wrapping struct {
	cause error
	msg   string
	stack *stack
}

func (w *wrapping) Error() string { return w.msg + ": " + w.cause.Error() }
func (w *wrapping) Cause() error  { return w.cause }

func (w *wrapping) Format(s fmt.State, verb rune) {
	if rune('v') == verb {
		panicIfError(fmt.Fprintf(s, "%v\n", w.Cause()))
		panicIfError(io.WriteString(s, w.msg))
		if getLogErrStacks() {
			w.stack.Format(s, verb)
		}
		return
	}

	if rune('s') == verb || rune('q') == verb {
		panicIfError(io.WriteString(s, w.Error()))
	}
}

// since we can't return an error, let's panic if something goes wrong here
func panicIfError(_ int, err error) {
	if err != nil {
		panic(err)
	}
}

// RootCause returns the underlying cause of the error, if possible.
// An error value has a cause if it implements the following
// interface:
//
//	type causer interface {
//	       Cause() error
//	}
//
// If the error does not implement Cause, the original error will
// be returned. If the error is nil, nil will be returned without further
// investigation.
func RootCause(err error) error {
	for {
		cause := Cause(err)
		if cause == nil {
			return err
		}
		err = cause
	}
}

// Cause will return the immediate cause, if possible.
// An error value has a cause if it implements the following
// interface:
//
//	type causer interface {
//	       Cause() error
//	}
//
// If the error does not implement Cause, nil will be returned
func Cause(err error) error {
	type causer interface {
		Cause() error
	}

	causerObj, ok := err.(causer)
	if !ok {
		return nil
	}

	return causerObj.Cause()
}

// Equals returns true iff the error message and the code returned by Code()
// are equal.
func Equals(a, b error) bool {
	if a == nil && b == nil {
		// Both are nil.
		return true
	}

	if a == nil || b == nil {
		// One of the two is nil, since we know both are not nil.
		return false
	}

	return a.Error() == b.Error() && Code(a) == Code(b)
}

// Print is meant to print the vtError object in test failures.
// For comparing two vterrors, use Equals() instead.
func Print(err error) string {
	return fmt.Sprintf("%v: %v\n", Code(err), err.Error())
}

// TruncateError truncates error messages that are longer than the
// specified length.
func TruncateError(oldErr error, max int) error {
	if oldErr == nil || max <= 0 || len(oldErr.Error()) <= max {
		return oldErr
	}

	if max <= 12 {
		return New(Code(oldErr), "[TRUNCATED]")
	}

	return New(Code(oldErr), oldErr.Error()[:max-12]+" [TRUNCATED]")
}

func (f *fundamental) ErrorState() State       { return f.state }
func (f *fundamental) ErrorCode() vtrpcpb.Code { return f.code }
