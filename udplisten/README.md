# UDPListen

Package udplisten provides customizable UDP net.PacketConn with various
performance-related options:

 * SO_REUSEPORT. This option allows linear scaling server performance
   on multi-CPU servers.
   See https://www.nginx.com/blog/socket-sharding-nginx-release-1-9-1/ for details.

 * SO_RCVBUF and SO_SNDBUF. These options allow tuning socket buffer sizes
   for receive and send operations.
