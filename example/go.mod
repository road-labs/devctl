// The example is its own module so the main devctl build and tests never reach
// into it. Its services depend on nothing but the standard library.
module devctl.example

go 1.23
