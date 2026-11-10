// Linux stand-in for the constants AudioFileFormat.refine names.
public typealias AudioFormatID = UInt32
public let kAudioFormatAppleLossless: AudioFormatID = 0x616c_6163 // 'alac'
public let kAudioFormatMPEG4AAC: AudioFormatID = 0x6161_6320 // 'aac '
public let kAudioFormatMPEG4AAC_HE: AudioFormatID = 0x6161_6368 // 'aach'
public let kAudioFormatMPEG4AAC_HE_V2: AudioFormatID = 0x6161_6370 // 'aacp'
public let kAudioFormatMPEG4AAC_LD: AudioFormatID = 0x6161_636c // 'aacl'
public let kAudioFormatMPEG4AAC_ELD: AudioFormatID = 0x6161_6365 // 'aace'

public struct AudioStreamBasicDescription: Sendable {
    public var mFormatID: AudioFormatID = 0
    public var mBitsPerChannel: UInt32 = 0
}
