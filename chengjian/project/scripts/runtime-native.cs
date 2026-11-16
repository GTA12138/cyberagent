// Dedicated hidden console: CTRL_BREAK reaches this app, never the user's shell.
using System;
using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;
public static class ChengjianNative {
    [StructLayout(LayoutKind.Sequential)] struct Security {
        public int Length; public IntPtr Descriptor;
        [MarshalAs(UnmanagedType.Bool)] public bool Inherit;
    }
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] struct Startup {
        public int Size; public string Reserved, Desktop, Title;
        public int X, Y, XSize, YSize, XChars, YChars, Fill, Flags;
        public short Show, ReservedSize; public IntPtr ReservedData, Input, Output, Error;
    }
    [StructLayout(LayoutKind.Sequential)] struct ProcessInfo {
        public IntPtr Process, Thread; public int ProcessId, ThreadId;
    }
    delegate bool Handler(uint signal);
    static Handler ignore = delegate(uint signal) { return true; };
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    static extern IntPtr CreateFile(string name, uint access, uint share, ref Security sa, uint disposition, uint flags, IntPtr template);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    static extern bool CreateProcess(string exe, StringBuilder args, IntPtr pa, IntPtr ta, bool inherit, uint flags, IntPtr env, string cwd, ref Startup si, out ProcessInfo pi);
    [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
    [DllImport("kernel32.dll")] static extern bool FreeConsole();
    [DllImport("kernel32.dll", SetLastError = true)] static extern bool AttachConsole(uint process);
    [DllImport("kernel32.dll", SetLastError = true)] static extern bool GenerateConsoleCtrlEvent(uint type, uint group);
    [DllImport("kernel32.dll")] static extern bool SetConsoleCtrlHandler(Handler handler, bool add);
    static IntPtr File(string path, uint access, uint disposition) {
        var sa = new Security { Length = Marshal.SizeOf(typeof(Security)), Inherit = true };
        var h = CreateFile(path, access, 3, ref sa, disposition, 0x80, IntPtr.Zero);
        if (h == new IntPtr(-1)) throw new Win32Exception(Marshal.GetLastWin32Error());
        return h;
    }
    public static Process Launch(string exe, string arguments, string cwd, string stdout, string stderr) {
        IntPtr output = IntPtr.Zero, error = IntPtr.Zero, input = IntPtr.Zero;
        try {
            output = File(stdout, 0x40000000, 2); error = File(stderr, 0x40000000, 2); input = File("NUL", 0x80000000, 3);
            var si = new Startup { Size = Marshal.SizeOf(typeof(Startup)), Flags = 0x101, Show = 0, Input = input, Output = output, Error = error };
            ProcessInfo pi;
            if (!CreateProcess(exe, new StringBuilder("\"" + exe + "\" " + arguments), IntPtr.Zero, IntPtr.Zero, true, 0x10, IntPtr.Zero, cwd, ref si, out pi))
                throw new Win32Exception(Marshal.GetLastWin32Error());
            try {
                var process = Process.GetProcessById(pi.ProcessId);
                var retainedHandle = process.Handle;
                return process;
            } finally { CloseHandle(pi.Thread); CloseHandle(pi.Process); }
        } finally {
            if (input != IntPtr.Zero) CloseHandle(input);
            if (output != IntPtr.Zero) CloseHandle(output);
            if (error != IntPtr.Zero) CloseHandle(error);
        }
    }
    // Called only from a short-lived helper, never in the user's PowerShell.
    public static void SendBreak(int processId) {
        FreeConsole();
        if (!AttachConsole((uint)processId)) throw new Win32Exception(Marshal.GetLastWin32Error());
        try {
            SetConsoleCtrlHandler(ignore, true);
            if (!GenerateConsoleCtrlEvent(1, 0)) throw new Win32Exception(Marshal.GetLastWin32Error());
            System.Threading.Thread.Sleep(150);
        } finally { FreeConsole(); }
    }
}
