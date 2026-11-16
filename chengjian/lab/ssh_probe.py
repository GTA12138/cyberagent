import socket

s = socket.create_connection(("127.0.0.2", 2222), 5)
print("SSH banner:", s.recv(64)[:30])
s.close()
