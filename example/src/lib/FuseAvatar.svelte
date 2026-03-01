<script lang="ts">
	import { onMount } from 'svelte';

	import type { HTMLCanvasAttributes } from 'svelte/elements';

	interface Props extends HTMLCanvasAttributes {
		/** base url of the fuse server */
		url?: string;
		/** ms between reconnection attempts */
		reconnectDelay?: number;
		/** fires when webrtc connection state changes */
		onstate?: (state: 'connecting' | 'connected' | 'disconnected') => void;
	}

	let { url = 'http://localhost:9090', reconnectDelay = 3000, onstate, ...rest }: Props = $props();

	let canvas: HTMLCanvasElement;

	onMount(() => {
		const video = document.createElement('video');
		video.autoplay = true;
		video.muted = true;
		video.playsInline = true;
		video.style.display = 'none';
		document.body.appendChild(video);

		const gl = initWebGL();
		if (!gl) return;

		let pc: RTCPeerConnection | null = null;
		let dead = false;

		async function connect() {
			if (dead) return;

			// tear down previous connection
			if (pc) {
				pc.onconnectionstatechange = null;
				pc.ontrack = null;
				pc.close();
				pc = null;
			}

			onstate?.('connecting');

			const conn = new RTCPeerConnection();
			pc = conn;

			conn.addTransceiver('video', { direction: 'recvonly' });

			conn.ontrack = (e) => {
				video.srcObject = e.streams[0];
				video.play();
				onstate?.('connected');
			};

			conn.onconnectionstatechange = () => {
				if (conn !== pc || dead) return;
				const s = conn.connectionState;

				if (s === 'failed' || s === 'closed') {
					onstate?.('disconnected');
					setTimeout(connect, reconnectDelay);
				} else if (s === 'disconnected') {
					onstate?.('disconnected');
					// give it a sec, disconnected can recover on its own
					setTimeout(() => {
						if (conn === pc && conn.connectionState === 'disconnected') connect();
					}, 5000);
				}
			};

			const offer = await conn.createOffer();
			await conn.setLocalDescription(offer);

			try {
				const resp = await fetch(`${url}/offer`, {
					method: 'POST',
					headers: { 'Content-Type': 'application/json' },
					body: JSON.stringify(conn.localDescription),
				});
				if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
				await conn.setRemoteDescription(await resp.json());
			} catch {
				onstate?.('disconnected');
				setTimeout(connect, reconnectDelay);
			}
		}

		// -- shaders --

		const VERT = `#version 300 es
			const vec2 pos[4] = vec2[](vec2(-1,-1), vec2(1,-1), vec2(-1,1), vec2(1,1));
			const vec2 uv[4]  = vec2[](vec2(0,1),   vec2(1,1),  vec2(0,0),  vec2(1,0));
			out vec2 vUV;
			void main() {
				gl_Position = vec4(pos[gl_VertexID], 0.0, 1.0);
				vUV = uv[gl_VertexID];
			}
		`;

		const FRAG = `#version 300 es
			precision highp float;
			uniform sampler2D uVideo;
			in vec2 vUV;
			out vec4 fragColor;
			void main() {
				vec3 color = texture(uVideo, vec2(vUV.x, vUV.y * 0.5)).rgb;
				float alpha = texture(uVideo, vec2(vUV.x, vUV.y * 0.5 + 0.5)).r;
				fragColor = vec4(color, alpha);
			}
		`;

		function initWebGL() {
			const ctx = canvas.getContext('webgl2', { alpha: true, premultipliedAlpha: false });
			if (!ctx) {
				console.error('fuse: webgl2 not supported');
				return null;
			}

			const vs = compile(ctx, ctx.VERTEX_SHADER, VERT);
			const fs = compile(ctx, ctx.FRAGMENT_SHADER, FRAG);
			if (!vs || !fs) return null;

			const prog = ctx.createProgram()!;
			ctx.attachShader(prog, vs);
			ctx.attachShader(prog, fs);
			ctx.linkProgram(prog);

			if (!ctx.getProgramParameter(prog, ctx.LINK_STATUS)) {
				console.error('fuse: shader link failed', ctx.getProgramInfoLog(prog));
				return null;
			}

			ctx.useProgram(prog);

			// video texture
			const tex = ctx.createTexture();
			ctx.activeTexture(ctx.TEXTURE0);
			ctx.bindTexture(ctx.TEXTURE_2D, tex);
			ctx.texParameteri(ctx.TEXTURE_2D, ctx.TEXTURE_WRAP_S, ctx.CLAMP_TO_EDGE);
			ctx.texParameteri(ctx.TEXTURE_2D, ctx.TEXTURE_WRAP_T, ctx.CLAMP_TO_EDGE);
			ctx.texParameteri(ctx.TEXTURE_2D, ctx.TEXTURE_MIN_FILTER, ctx.LINEAR);
			ctx.texParameteri(ctx.TEXTURE_2D, ctx.TEXTURE_MAG_FILTER, ctx.LINEAR);
			ctx.uniform1i(ctx.getUniformLocation(prog, 'uVideo'), 0);

			// transparency
			ctx.enable(ctx.BLEND);
			ctx.blendFunc(ctx.SRC_ALPHA, ctx.ONE_MINUS_SRC_ALPHA);
			ctx.bindVertexArray(ctx.createVertexArray());

			return { ctx, tex };
		}

		function compile(ctx: WebGL2RenderingContext, type: number, source: string) {
			const s = ctx.createShader(type)!;
			ctx.shaderSource(s, source);
			ctx.compileShader(s);
			if (!ctx.getShaderParameter(s, ctx.COMPILE_STATUS)) {
				console.error('fuse: shader compile failed', ctx.getShaderInfoLog(s));
				return null;
			}
			return s;
		}

		// -- render loop --

		const { ctx, tex } = gl;
		let raf: number;

		function render() {
			if (dead) return;

			if (video.readyState >= video.HAVE_CURRENT_DATA && video.videoWidth > 0) {
				const w = video.videoWidth;
				const h = video.videoHeight / 2; // content is half the stacked frame

				if (canvas.width !== w || canvas.height !== h) {
					canvas.width = w;
					canvas.height = h;
					ctx.viewport(0, 0, w, h);
				}

				ctx.bindTexture(ctx.TEXTURE_2D, tex);
				ctx.texImage2D(ctx.TEXTURE_2D, 0, ctx.RGBA, ctx.RGBA, ctx.UNSIGNED_BYTE, video);
				ctx.clearColor(0, 0, 0, 0);
				ctx.clear(ctx.COLOR_BUFFER_BIT);
				ctx.drawArrays(ctx.TRIANGLE_STRIP, 0, 4);
			}

			raf = requestAnimationFrame(render);
		}

		connect();
		raf = requestAnimationFrame(render);

		// cleanup
		return () => {
			dead = true;
			cancelAnimationFrame(raf);
			if (pc) {
				pc.onconnectionstatechange = null;
				pc.ontrack = null;
				pc.close();
			}
			video.remove();
		};
	});
</script>

<canvas bind:this={canvas} style="background: transparent;" {...rest}></canvas>
