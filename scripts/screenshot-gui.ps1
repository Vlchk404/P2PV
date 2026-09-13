# Captures the P2PV window to a PNG, for checking the layout without a human
# looking at the screen.
#
# It shoots build\p2pv-gui-test.exe, not build\P2PV.exe. The shipped exe demands
# administrator rights in its manifest, so it cannot be launched from an
# unelevated session at all. The twin is the same code built with the
# as-invoker manifest from winres-test/:
#
#   cp cmd/p2pv-gui/rsrc_windows_amd64.syso /tmp/admin.bak
#   (cd winres-test && go-winres make --out ../cmd/p2pv-gui/rsrc)
#   go build -ldflags="-H windowsgui" -o build/p2pv-gui-test.exe ./cmd/p2pv-gui
#   cp /tmp/admin.bak cmd/p2pv-gui/rsrc_windows_amd64.syso   # put the real one back
#
# Restoring the admin .syso matters: forget it and the next build of P2PV.exe
# silently ships without elevation and fails when it tries to create the adapter.

Add-Type -AssemblyName System.Windows.Forms,System.Drawing
Add-Type @"
using System;
using System.Text;
using System.Collections.Generic;
using System.Drawing;
using System.Runtime.InteropServices;
public class W {
  public delegate bool Proc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] public static extern bool EnumWindows(Proc p, IntPtr l);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint p);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr h, IntPtr dc, uint flags);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L,T,R,B; }

  // Found by owning process and size rather than by title: FindWindow needs an
  // exact match and the tray icon owns same-titled helper windows.
  public static IntPtr MainOf(uint pid) {
    IntPtr best = IntPtr.Zero;
    int bestArea = 0;
    EnumWindows((h, l) => {
      uint wp; GetWindowThreadProcessId(h, out wp);
      if (wp == pid && IsWindowVisible(h)) {
        RECT r; GetWindowRect(h, out r);
        int area = (r.R - r.L) * (r.B - r.T);
        if (area > bestArea) { bestArea = area; best = h; }
      }
      return true;
    }, IntPtr.Zero);
    return best;
  }

  // PrintWindow asks the window to paint itself into our bitmap, so the capture
  // is correct even when it is behind something else. CopyFromScreen cannot do
  // that: a fullscreen game will not yield the foreground, and the screen grab
  // then silently returns the game's pixels instead of ours.
  // Flag 2 is PW_RENDERFULLCONTENT, needed for DirectComposition-rendered parts.
  public static Bitmap Capture(IntPtr h) {
    RECT r; GetWindowRect(h, out r);
    var bmp = new Bitmap(r.R - r.L, r.B - r.T);
    using (var g = Graphics.FromImage(bmp)) {
      IntPtr dc = g.GetHdc();
      PrintWindow(h, dc, 2);
      g.ReleaseHdc(dc);
    }
    return bmp;
  }
}
"@ -ReferencedAssemblies System.Drawing

# A fresh config directory per run, so the capture shows a genuine first launch
# instead of whatever networks an earlier run left behind.
$env:APPDATA = Join-Path $env:TEMP "p2pv-cfg-$(Get-Random)"
$proc = Start-Process -FilePath 'build\p2pv-gui-test.exe' -PassThru
Start-Sleep -Seconds 4

$h = [W]::MainOf([uint32]$proc.Id)
if ($h -eq [IntPtr]::Zero) {
    Write-Host 'WINDOW NOT FOUND'
    $proc | Stop-Process -Force
    exit 1
}

$r = New-Object W+RECT
[W]::GetWindowRect($h, [ref]$r) | Out-Null
Write-Host "window: $($r.R - $r.L)x$($r.B - $r.T)"

$bmp = [W]::Capture($h)
$out = Join-Path $env:TEMP 'p2pv-gui.png'
$bmp.Save($out, [Drawing.Imaging.ImageFormat]::Png)
Write-Host "saved: $out"
$bmp.Dispose()
$proc | Stop-Process -Force
Remove-Item -Recurse -Force $env:APPDATA -ErrorAction SilentlyContinue
