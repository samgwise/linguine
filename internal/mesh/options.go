package mesh

import "time"

// recvDeadline bounds every Recv call so a topology test never hangs
// indefinitely on a missed message.
const recvDeadline = 2 * time.Second
