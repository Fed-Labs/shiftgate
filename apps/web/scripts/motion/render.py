#!/usr/bin/env python3
import math, os, shutil, subprocess, tempfile
from pathlib import Path

W,H,FPS,FRAMES=1920,1080,30,1050
OUT=Path(__file__).resolve().parents[2]/'public/media'
TMP=Path(tempfile.mkdtemp(prefix='shift-film-'))
FONT='/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf'
MONO='/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf'

def ease(x): return x*x*(3-2*x)
def clamp(x,a=0,b=1): return max(a,min(b,x))
def opacity(start,end,t): return clamp((t-start)/(end-start)) if end>start else 1

def txt(x,y,s,size,fill='#e9e7e2',family='DejaVu Sans',weight='400',anchor='start',letter='0'):
    return f'<text x="{x}" y="{y}" font-family="{family}" font-size="{size}px" font-weight="{weight}" letter-spacing="{letter}px" fill="{fill}" text-anchor="{anchor}">{s}</text>'
def rect(x,y,w,h,stroke='#26262c',fill='none',sw=2,op=1,rx=0):
    return f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{rx}" fill="{fill}" stroke="{stroke}" stroke-width="{sw}" opacity="{op}"/>'
def line(x1,y1,x2,y2,stroke='#26262c',sw=2,op=1):
    return f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{stroke}" stroke-width="{sw}" opacity="{op}"/>'

def frame(i):
    t=i/FPS
    s=['<svg xmlns="http://www.w3.org/2000/svg" width="1920" height="1080" viewBox="0 0 1920 1080">','<rect width="1920" height="1080" fill="#0a0a0b"/>']
    for x in range(0,W+1,80): s.append(line(x,0,x,H,'#19191e',1,.32))
    for y in range(0,H+1,80): s.append(line(0,y,W,y,'#19191e',1,.32))
    s.append(txt(100,100,'SHIFTGATE',24,'#14e0b0','DejaVu Sans Mono','500','start','3'))
    s.append(txt(1820,100,'CONTINUITY / 001',16,'#63636a','DejaVu Sans Mono','400','end','2'))
    if t < 4.67:
        o=opacity(0.1,1,t); s.append(txt(100,290,'MOVE',132,'#e9e7e2','DejaVu Sans','600')); s.append(txt(100,425,'THE WORK',132,'#14e0b0','DejaVu Sans','600')); s.append(txt(104,500,'A running workload has state.',25,'#9c9c9f','DejaVu Sans','400'))
    else:
        # source/destination frames
        left,right,top,bottom=120,1800,350,820
        s.append(txt(120,290,'SOURCE',16,'#63636a','DejaVu Sans Mono','500','start','3'))
        s.append(txt(1800,290,'DESTINATION',16,'#63636a','DejaVu Sans Mono','500','end','3'))
        s.append(rect(left,top,520,bottom-top,'#3a3a42','#101012',2,.95))
        s.append(rect(right-520,top,520,bottom-top,'#3a3a42','#101012',2,.95))
        s.append(line(640,585,1280,585,'#3a3a42',2,.9))
        # states build between 4 and 16 seconds
        layers=['PROCESS','MEMORY','FILESYSTEM','NETWORK','DEVICE','ENVIRONMENT']
        build=clamp((t-4.67)/7.0)
        for n,label in enumerate(layers):
            y=430+n*48; amount=clamp(build*1.15-n*.1)
            s.append(txt(180,y+9,label,13,'#63636a','DejaVu Sans Mono','500','start','1'))
            s.append(rect(180,y+20,350,10,'#26262c','#16161a',1,.9))
            s.append(rect(180,y+20,350*amount,10,'#14e0b0','#14e0b0',0,amount))
            s.append(txt(1740,y+9,label,13,'#63636a','DejaVu Sans Mono','500','end','1'))
        # source active core, destination starts after travel
        source_o=1 if t<25.67 else clamp(1-(t-25.67)/3.5)
        dest_o=clamp((t-21)/5.8)
        for cx,cy,o in [(380,670,source_o),(1540,670,dest_o)]:
            s.append(rect(cx-100,cy-45,200,90,'#14e0b0','none',2,o))
            s.append(line(cx-70,cy,cx+70,cy,'#14e0b0',8,o))
            s.append(line(cx,cy-25,cx,cy+25,'#14e0b0',8,o))
        if 11.67<=t<26.83:
            s.append(txt(960,290,'CAPTURED STATE',20,'#14e0b0','DejaVu Sans Mono','500','middle','3'))
            s.append(txt(960,315,'encrypted / content-addressed',14,'#63636a','DejaVu Sans Mono','400','middle','1'))
        if t>=18.67:
            p=clamp((t-18.67)/8.17)
            # deterministic packet rail
            for k in range(7):
                q=(p*1.2+k*.13)%1
                x=660+q*600
                s.append(rect(x,575,18,18,'#14e0b0','#14e0b0',0,clamp(1-abs(q-.5)*1.4)))
            s.append(txt(960,650,'CHECKPOINT  →  TRANSFER  →  VERIFY  →  RESTORE',16,'#9c9c9f','DejaVu Sans Mono','500','middle','2'))
        if t>=26.83:
            s.append(txt(960,710,'compatible target / health validated',18,'#14e0b0','DejaVu Sans Mono','500','middle','2'))
        # The checkpoint becomes a primitive: branch into the product's useful verbs.
        if 28.0 <= t < 31.5:
            branch = clamp((t-28.0)/1.4)
            origin_x, origin_y = 960, 760
            nodes = [(520,900,'CLONE','#14e0b0'), (960,930,'PARK','#d6a83b'), (1400,900,'FAILOVER','#e9e7e2')]
            for nx, ny, label, color in nodes:
                s.append(line(origin_x, origin_y, origin_x+(nx-origin_x)*branch, origin_y+(ny-origin_y)*branch, color, 2, .82))
                node_o = clamp((t-29.0)/.8)
                s.append(rect(nx-72,ny-22,144,44,color,'#101012',2,node_o))
                s.append(txt(nx,ny+6,label,13,color,'DejaVu Sans Mono','500','middle','2'))
            s.append(txt(960,820,'ONE VERIFIED CHECKPOINT / MANY RECOVERY PATHS',13,'#63636a','DejaVu Sans Mono','500','middle','2'))
    if t>=31.5:
        final_o = clamp((t-31.5)/1.0)
        s.append(rect(80,900,1760,1,'#14e0b0','#14e0b0',0,final_o))
        s.append(txt(100,980,'MOVE COMPUTATION. KEEP THE STATE.',30,'#e9e7e2','DejaVu Sans','600','start','1'))
        s.append(txt(1820,980,'LINUX X86_64 / CRIU-BASED / COMPATIBILITY-CHECKED',14,'#63636a','DejaVu Sans Mono','400','end','1'))
    s.append('</svg>')
    return ''.join(s)

for i in range(FRAMES):
    (TMP/f'frame-{i:04d}.svg').write_text(frame(i), encoding='utf-8')
    subprocess.run(['chromium','--headless','--no-sandbox','--disable-gpu','--hide-scrollbars','--run-all-compositor-stages-before-draw','--window-size=1920,1080',f'--screenshot={TMP/f"frame-{i:04d}.png"}',f'file://{TMP/f"frame-{i:04d}.svg"}'], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
OUT.mkdir(parents=True, exist_ok=True)
subprocess.run(['ffmpeg','-y','-loglevel','error','-framerate','30','-i',str(TMP/'frame-%04d.png'),'-c:v','libx264','-preset','slow','-crf','20','-pix_fmt','yuv420p','-movflags','+faststart',str(OUT/'shiftgate-continuity.mp4')], check=True)
subprocess.run(['ffmpeg','-y','-loglevel','error','-framerate','30','-i',str(TMP/'frame-0000.png'),'-frames:v','1','-c:v','libwebp','-quality','90',str(OUT/'shiftgate-continuity-poster.webp')], check=True)
subprocess.run(['ffmpeg','-y','-loglevel','error','-framerate','30','-i',str(TMP/'frame-%04d.png'),'-c:v','libvpx-vp9','-b:v','0','-crf','32','-pix_fmt','yuv420p','-an',str(OUT/'shiftgate-continuity.webm')], check=True)
shutil.rmtree(TMP)
