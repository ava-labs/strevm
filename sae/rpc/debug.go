package rpc

func (b *apiBackend) SetHead(uint64) {
	b.vm.Logger().Info("debug_setHead called but not supported by SAE")
}
