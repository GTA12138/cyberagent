import asyncio
import asyncssh


async def main():
    try:
        async with asyncssh.connect("127.0.0.2", 2222, username="root",
                                    password="chengjian123", known_hosts=None) as conn:
            result = await conn.run("whoami", check=False)
            print("stdout:", result.stdout.strip())
            print("exit:", result.exit_status)
    except Exception as e:  # noqa: BLE001
        print(f"FAIL: {type(e).__name__}: {e}")


asyncio.run(main())
