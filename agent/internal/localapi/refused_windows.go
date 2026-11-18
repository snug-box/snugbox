package localapi

import "golang.org/x/sys/windows"

// refused are the errors a dial gets when nothing listens at the socket's
// path. Windows reports a missing socket file as a refused connection.
var refused = []error{windows.WSAECONNREFUSED}
