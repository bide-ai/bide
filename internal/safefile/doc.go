// Package safefile opens files named by an operator or a host without letting a named pipe stop the
// program: the open does not wait for a writer, and the caller then asks the opened file (not the
// path, which may have changed since it was checked) whether it is a regular file.
package safefile
