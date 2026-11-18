//go:build !darwin || !cgo

package main

const haveWindow = false

func runWindow(url string) {}
