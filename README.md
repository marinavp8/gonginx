# gonginx

Clon mínimo de nginx escrito en Go. Lee un `nginx.conf` con sintaxis nginx.

- Ficheros estáticos (`root`, `index`, `try_files`), `return`, `add_header`
- Hosts virtuales, `location` (`=`, `^~`, prefijo, regex), TLS con SNI
- Proxy inverso con `upstream` (round-robin y pesos) y sockets Unix
- Bloque `stream`: proxy TCP y UDP
- Recarga con `SIGHUP`

```sh
go build -o gonginx . && ./gonginx -t -c nginx.conf && ./gonginx -c nginx.conf
```
