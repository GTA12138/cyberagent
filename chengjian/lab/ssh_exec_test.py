import paramiko

c = paramiko.SSHClient()
c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
c.connect("127.0.0.2", 2222, username="root", password="chengjian123", timeout=5)
_, out, err = c.exec_command("whoami && hostname", timeout=5)
print("stdout:", out.read().decode().strip())
print("exit:", out.channel.recv_exit_status())
c.close()
