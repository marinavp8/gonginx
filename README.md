# gonginx

Clon mínimo de nginx escrito en Go. Lee un `nginx.conf` con la misma sintaxis y sirve ficheros estáticos, hace de proxy inverso con balanceo y reenvía tráfico TCP y UDP, con recarga en caliente.

Es un proyecto para aprender cómo funciona un servidor web por dentro y para uso propio. No pretende sustituir a nginx en producción.

## Características

- **Servidor HTTP**: ficheros estáticos (`root`, `index`, `try_files`), `return`, `add_header`, log de acceso.
- **Hosts virtuales** por `server_name` (nombre exacto y comodín `*.dominio`) y `default_server`.
- **Selección de `location`** con el mismo orden que nginx: `=`, prefijo más largo, `^~`, regex `~` y `~*`.
- **Proxy inverso** con `upstream`, reparto round-robin con pesos (`weight=N`) y servidores `down`.
- **TLS** con certificados por host virtual (SNI) y HTTP/2.
- **Sockets Unix** como destino de proxy, tanto en HTTP como en stream.
- **Módulo `stream`**: proxy de capa 4 para TCP y UDP (SSH, bases de datos, DNS...).
- **Recarga en caliente** con `SIGHUP`, sin cortar las conexiones activas.
- **Validación** de la configuración con `-t`.

## Instalación y uso

Requiere Go 1.27 (la versión fijada en `go.mod`; con un Go más antiguo, la toolchain se descarga sola).

```sh
git clone https://github.com/marinavp8/gonginx.git
cd gonginx
go build -o gonginx .

./gonginx -t -c nginx.conf   # comprueba la sintaxis y sale
./gonginx -c nginx.conf      # arranca el servidor
kill -HUP <pid>              # recarga la configuración
```

Opciones:

| Opción | Descripción |
|---|---|
| `-c <fichero>` | Fichero de configuración (por defecto `nginx.conf`). |
| `-t` | Valida la configuración y sale. |

Señales:

| Señal | Efecto |
|---|---|
| `SIGHUP` | Recarga la configuración. Si es inválida, se mantiene la anterior. |
| `SIGINT`, `SIGTERM` | Apagado ordenado (hasta 10 s para terminar peticiones en curso). |

## Ejemplo de configuración

```nginx
http {
    upstream backend {
        server 127.0.0.1:9001;
        server 127.0.0.1:9002 weight=2;
        server unix:/var/run/app.sock;
    }

    server {
        listen 8080;
        server_name localhost;
        root html;

        location / {
            try_files $uri $uri/ =404;
        }

        # /api/x llega al backend como /x
        location /api/ {
            proxy_pass http://backend/;
            proxy_set_header X-Real-IP $remote_addr;
        }

        # Socket Unix directo
        location /socket/ {
            proxy_pass http://unix:/var/run/app.sock:/;
        }

        location = /health { return 200 "ok"; }
    }
}

stream {
    upstream ssh_pool { server 192.168.1.38:22; }

    server { listen 2222;     proxy_pass ssh_pool;   proxy_timeout 10m; }
    server { listen 5454 udp; proxy_pass 8.8.8.8:53; proxy_timeout 20s; }
}
```

## Directivas soportadas

| Ámbito | Directivas |
|---|---|
| Servidor | `listen`, `server_name`, `root`, `index`, `try_files`, `access_log off`, `include` |
| Respuesta | `return`, `add_header` |
| Proxy HTTP | `proxy_pass`, `proxy_set_header` |
| Upstream | `upstream`, `server` (con `weight=N` y `down`) |
| TLS | `listen ... ssl`, `ssl_certificate`, `ssl_certificate_key`, `default_server` |
| Stream | `stream`, `listen ... udp`, `proxy_pass`, `proxy_timeout`, `proxy_connect_timeout` |

Variables disponibles en `return`, `add_header` y `proxy_set_header`: `$host`, `$http_host`, `$uri`, `$args`, `$request_uri`, `$remote_addr` y `$scheme`.

Las directivas desconocidas se ignoran sin avisar.

### Orden de las `location`

1. `= /ruta`: coincidencia exacta. Termina la búsqueda.
2. `/ruta` y `^~ /ruta`: gana el prefijo más largo. Con `^~` se corta aquí y no se miran las regex.
3. `~` y `~*`: expresiones regulares en orden de aparición (`~*` ignora mayúsculas).
4. Si ninguna regex acierta, se usa el prefijo más largo del paso 2.

### Sockets Unix

- En `proxy_pass` HTTP: `http://unix:/ruta/app.sock:/uri`.
- En un `upstream`: `server unix:/ruta/app.sock;`.
- En `stream`: `proxy_pass unix:/ruta/app.sock;` (solo TCP).

## Cómo funciona

Cada `listen` abre un puerto. Según el bloque donde esté, una conexión sigue uno de dos caminos:

```
HTTP    cliente → listener (:8080, TLS/SNI) → host virtual → location → estático | proxy | return
STREAM  cliente → listener (TCP/UDP) → upstream (round-robin) → copia bidireccional → destino
```

| Fichero | Responsabilidad |
|---|---|
| `config.go` | Tokenizador y parser de la sintaxis nginx, `include` y herencia de directivas (`location` → `server` → `http`). |
| `handler.go` | Hosts virtuales, selección de `location`, ficheros estáticos, proxy inverso, upstreams y log de acceso. |
| `stream.go` | Proxy de capa 4: listeners TCP y UDP, sesiones UDP con caducidad, timeouts. |
| `unix.go` | Destinos `unix:/ruta`: el socket se codifica como un host ficticio y el `DialContext` del transporte lo decodifica. |
| `main.go` | Construye la configuración, arranca y cierra listeners, recarga con `SIGHUP` y apagado ordenado. |

Detalles de diseño:

- **Una goroutine por conexión.** El código de `stream` es secuencial: `Accept`, `Dial`, copia en los dos sentidos.
- **Recarga sin cortar conexiones.** Cada listener guarda su configuración en un `atomic.Pointer`. Al recargar se sustituye el puntero; las conexiones ya abiertas siguen con la configuración anterior. Los listeners que desaparecen se cierran y los nuevos se abren.
- **Stream TCP.** Cuando un lado cierra, se cierran los dos, como nginx sin `proxy_half_close`. `proxy_timeout` actúa como tiempo máximo de inactividad.
- **Stream UDP.** Hay una sesión por cliente, con un socket hacia el destino, que caduca tras `proxy_timeout` sin actividad.
- **Rutas seguras.** Las rutas se normalizan antes de buscar el fichero, de modo que `../` no puede salir del `root`.

## Pruebas realizadas

Pruebas manuales con servidores de prueba locales. Todavía no hay tests automatizados.

| Prueba | Resultado |
|---|---|
| Estáticos, `return`, redirección y 404 | Correcto |
| Balanceo HTTP con pesos 1:2 | 2 y 4 de 6 peticiones |
| Ruta con `../../etc/passwd` | 404, no sale del `root` |
| Recarga con `SIGHUP` | Correcto |
| HTTP a socket Unix, directo y por `upstream` | Correcto |
| Stream TCP con dos backends | 2 y 2 de 4 conexiones |
| Stream TCP hacia un socket Unix | Correcto |
| Stream UDP con un servidor de eco | 3 de 3 respuestas |

**Sin probar todavía:** TLS con certificados reales y WebSockets.

## Limitaciones

- No hay `gzip`, caché de proxy, `limit_req`, `rewrite`, `autoindex`, `auth_basic` ni `error_page`.
- Los upstreams no tienen comprobaciones de salud ni fallo pasivo; solo hay round-robin con pesos.
- `stream` no soporta TLS (ni terminación ni `ssl_preread`). UDP hacia un socket Unix tampoco.
- Si cambia `ssl` on/off en un `listen` ya abierto, hace falta reiniciar.
- En macOS, el puerto UDP 5353 lo usa mDNS y no se puede reutilizar.

## Hoja de ruta

- Fallo pasivo y comprobaciones de salud; `least_conn` e `ip_hash`.
- `gzip`, `proxy_cache` y `limit_req`.
- `rewrite`, `autoindex`, `auth_basic` y `error_page`.
- Tests automatizados y benchmark contra nginx real con `wrk`.

## Por qué Go

- **Concurrencia sencilla** con goroutines y código secuencial.
- **Librería estándar muy completa**: HTTP/1.1 y HTTP/2, `ReverseProxy`, TLS con SNI, rangos y tipos MIME.
- **Memoria segura**, sin desbordamientos de búfer.
- **Binario único**, con compilación cruzada a Linux desde cualquier sistema.

A cambio, el rendimiento es algo menor que el de nginx con cientos de miles de conexiones simultáneas, hay un recolector de basura (con pausas muy cortas) y no existe el ecosistema de módulos de nginx.
