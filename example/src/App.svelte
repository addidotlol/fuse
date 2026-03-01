<script lang="ts">
	import FuseAvatar from '$lib/FuseAvatar.svelte';

	let status = $state<'connecting' | 'connected' | 'disconnected'>('connecting');
</script>

<main>
	<div class="status" class:connected={status === 'connected'}>
		<span class="dot"></span>
		{status}
	</div>

	<FuseAvatar
		url="http://localhost:9090"
		onstate={(s) => (status = s)}
		class="avatar"
	/>
</main>

<style>
	:global(body) {
		margin: 0;
		min-height: 100vh;
		display: flex;
		align-items: center;
		justify-content: center;
		background: repeating-conic-gradient(#808080 0% 25%, #a0a0a0 0% 50%) 0 0 / 20px 20px;
	}

	main {
		position: relative;
		width: 100%;
		height: 100vh;
		display: flex;
		align-items: center;
		justify-content: center;
	}

	.status {
		position: fixed;
		top: 10px;
		left: 10px;
		background: rgba(0, 0, 0, 0.7);
		color: #fff;
		padding: 8px 14px;
		border-radius: 6px;
		font-size: 13px;
		font-family: system-ui, sans-serif;
		z-index: 10;
		display: flex;
		align-items: center;
		gap: 6px;
	}

	.dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: #f44;
	}

	.status.connected .dot {
		background: #4f4;
	}

	:global(.avatar) {
		max-width: 90vw;
		max-height: 90vh;
	}
</style>
